package service

import (
	"fmt"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// usockWriter is an interface for writing USOCK messages (for testing)
type usockWriter interface {
	WriteWithFrameID(frameID byte, data []byte) error
}

// usockCloser extends usockWriter with Close capability
type usockCloser interface {
	usockWriter
	Close() error
}

// FirmwareUpdater interface for firmware update operations
type firmwareUpdaterInterface interface {
	CheckAndUpdate(currentVersion string) (bool, error)
	RecoverFromDFU() error
}

// Service represents the MDB Bluetooth service
type Service struct {
	usock           usockCloser
	ipc             *ipc.Client // Redis IPC client
	log             *logger.Logger
	stopCh          chan struct{}
	wg              sync.WaitGroup
	faults          *ipc.FaultReporter // Fault tracking
	serialDevice    string
	baudRate        int
	usockHandler    func(*usock.Payload)
	usockErrHandler func(error)
	firmwareUpdater firmwareUpdaterInterface
	autoUpdate      bool
	lastMileage     string // last mileage value sent to nRF (for dedup)
	mu              sync.RWMutex

	// Track the most recent vehicle state string for diagnostic/log paths.
	lastVehicleState string

	// Subscription management
	subStopCh chan struct{}
	subWg     sync.WaitGroup
	subMu     sync.Mutex

	// Serializes RestartSubscriptions: concurrent restarts (nRF reset resync
	// vs post-negotiation resync) would leak the first caller's watcher set,
	// leaving two subscriptions forwarding every event.
	restartMu sync.Mutex

	// nRF reset resync debounce: the firmware re-sends RESET_INFO until it is
	// ACKed, so several can arrive before our ACK lands; resync only once.
	resyncMu        sync.Mutex
	lastResetResync time.Time

	// Extended response rate limiting: the nRF SoftDevice's HVN TX queue
	// is 1 deep, so back-to-back notifications get dropped. Pace consecutive
	// extended responses to give the BLE link time to drain.
	extRespMu       sync.Mutex
	lastExtRespTime time.Time

	// Set when an LTC control command originated from a BLE extended
	// command, so the USOCK response handler knows to relay the result
	// back as an extended response.
	ltcBLEPending bool

	// Cached copy of the settings schema (Redis key `settings:schema`)
	// for the generic get:/set: extended commands. Lazy-loaded on first
	// use so bluetooth-service starting before settings-service still
	// recovers.
	schemaMu    sync.RWMutex
	schemaCache map[string]settingSchema

	// UART link manager: baud negotiation with the nRF and automatic fallback
	link *LinkManager

	// OTA receiver: phone -> scooter firmware transfer over the OTA tunnel
	ota otaReceiver
}

// otaReceiver is the subset of *ota.Receiver used by the USOCK dispatch
// (interface so the service compiles without a receiver wired, e.g. in tests)
type otaReceiver interface {
	HandleData(payload []byte)
	HandleControl(payload []byte)
}

// Fault codes for bluetooth service
const (
	FaultSerialPort     = 1 // Serial port communication error
	FaultNRFInit        = 2 // nRF52 initialization error
	FaultFirmwareUpdate = 3 // Firmware update error
)

// New creates a new Service instance
func New(ipcClient *ipc.Client, log *logger.Logger) *Service {
	s := &Service{
		ipc:    ipcClient,
		log:    log,
		stopCh: make(chan struct{}),
		faults: ipcClient.NewFaultReporter("ble"),
	}
	s.link = newLinkManager(s)
	return s
}

// SetOTAReceiver wires the OTA transfer receiver into the USOCK dispatch.
func (s *Service) SetOTAReceiver(r otaReceiver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ota = r
}

// Link returns the UART link manager.
func (s *Service) Link() *LinkManager {
	return s.link
}

// EnsureLinkBaud brings the UART link to the requested baud rate (used before
// nRF DFU, which only talks 115200).
func (s *Service) EnsureLinkBaud(baud int) error {
	return s.link.EnsureLinkBaud(baud)
}

// getUSockConcrete returns the underlying *usock.USOCK, or nil when the
// connection is absent or replaced by a test double.
func (s *Service) getUSockConcrete() *usock.USOCK {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sock, ok := s.usock.(*usock.USOCK); ok {
		return sock
	}
	return nil
}

// reopenUSockAtBaud closes the current serial connection and reopens it at the
// given baud rate, re-applying handlers. Unlike ReconnectUSock this does NOT
// run the nRF init sequence — it is the link manager's low-level primitive.
func (s *Service) reopenUSockAtBaud(baud int) error {
	s.mu.Lock()
	if s.serialDevice == "" || s.usockHandler == nil {
		s.mu.Unlock()
		return fmt.Errorf("serial config not set")
	}
	device := s.serialDevice
	handler := s.usockHandler
	errHandler := s.usockErrHandler
	old := s.usock
	s.mu.Unlock()

	if old != nil {
		if err := old.Close(); err != nil {
			s.log.Warnf("reopenUSockAtBaud: close failed: %v", err)
		}
	}

	sock, err := usock.New(device, baud, handler, s.log)
	if err != nil {
		return fmt.Errorf("failed to reopen serial port at %d baud: %w", baud, err)
	}
	if errHandler != nil {
		sock.SetErrorHandler(errHandler)
	}
	sock.SetSyncFrameIDs(ble.FrameOTAData, ble.FrameOTACtrl)

	s.mu.Lock()
	s.usock = sock
	s.mu.Unlock()
	return nil
}

// SetUSock sets the USOCK connection for the service
func (s *Service) SetUSock(sock *usock.USOCK) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usock = sock
}

// GetUSock returns the current USOCK connection for sending messages.
func (s *Service) GetUSock() usockWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usock
}

// SetSerialConfig stores the serial configuration for reconnection.
// errHandler is reapplied to the new USOCK after a reconnect so serial
// faults from the post-DFU connection still surface as FaultSerialPort.
func (s *Service) SetSerialConfig(device string, baud int, handler func(*usock.Payload), errHandler func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serialDevice = device
	s.baudRate = baud
	s.usockHandler = handler
	s.usockErrHandler = errHandler
}

// SetFirmwareUpdater sets the firmware updater for the service
func (s *Service) SetFirmwareUpdater(fu firmwareUpdaterInterface) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.firmwareUpdater = fu
}

// SetAutoUpdate sets whether automatic firmware updates are enabled
func (s *Service) SetAutoUpdate(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.autoUpdate = enabled
}

// GetAutoUpdate returns whether automatic firmware updates are enabled
func (s *Service) GetAutoUpdate() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.autoUpdate
}

// GetFirmwareUpdater returns the firmware updater
func (s *Service) GetFirmwareUpdater() firmwareUpdaterInterface {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.firmwareUpdater
}

// CloseUSock closes the serial connection
func (s *Service) CloseUSock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.usock != nil {
		return s.usock.Close()
	}
	return nil
}

// ReconnectUSock reopens the serial connection and re-initializes the nRF.
// Also clears the in-memory dedup caches so that post-DFU state pushes from
// SubscribeToRedisChannels' StartWithSync are not suppressed against values
// that were sent to the previous (now replaced) firmware image.
func (s *Service) ReconnectUSock() error {
	s.mu.Lock()
	if s.serialDevice == "" || s.usockHandler == nil {
		s.mu.Unlock()
		return fmt.Errorf("serial config not set")
	}
	device := s.serialDevice
	baud := s.baudRate
	handler := s.usockHandler
	errHandler := s.usockErrHandler
	s.lastMileage = ""
	s.lastVehicleState = ""
	s.mu.Unlock()

	// return port ownership to the link manager: the updater reaches this
	// point on every exit path (success and attemptReconnect). Resume before
	// the reopen attempt so a failed reopen can't leave the manager suspended
	// forever; nothing acts on the port until StartNegotiation below.
	s.link.resetToDefault()
	s.link.Resume()

	sock, err := usock.New(device, baud, handler, s.log)
	if err != nil {
		return fmt.Errorf("failed to reconnect to nRF52: %w", err)
	}
	if errHandler != nil {
		sock.SetErrorHandler(errHandler)
	}
	sock.SetSyncFrameIDs(ble.FrameOTAData, ble.FrameOTACtrl)

	s.mu.Lock()
	s.usock = sock
	s.mu.Unlock()

	if err := s.InitializeNRF52(); err != nil {
		return fmt.Errorf("failed to re-initialize nRF52: %w", err)
	}

	s.link.StartNegotiation()

	return nil
}

// SetFault marks a fault code active. The description reaches the
// events:faults stream, so it should name the specific failure rather than
// restate the code's category. Repeating a fault that is already active is a
// no-op and emits no stream entry.
func (s *Service) SetFault(code int, description string) {
	if err := s.faults.Raise(code, description); err != nil {
		s.log.Errorf("Failed to raise fault %d: %v", code, err)
	}
}

// ClearFault marks a fault code inactive.
func (s *Service) ClearFault(code int) {
	if err := s.faults.Clear(code); err != nil {
		s.log.Errorf("Failed to clear fault %d: %v", code, err)
	}
}

// Stop stops the service: stops Redis subscriptions, closes USOCK, signals
// other goroutines to exit. Safe to call when subscriptions or USOCK were
// never started.
func (s *Service) Stop() {
	s.StopSubscriptions()
	if err := s.CloseUSock(); err != nil {
		s.log.Errorf("Failed to close USOCK during shutdown: %v", err)
	}
	close(s.stopCh)
}

// Bootstrap probes the nRF for DFU mode and either runs bricked-app recovery
// or normal startup. Should be called once at service startup, after
// SetSerialConfig and SetFirmwareUpdater.
//
// If the bootloader responds to a SLIP version probe within the probe
// timeout, the device is stuck in DFU with no working application — we run
// the firmware updater's recovery path, which flashes the latest zip pair
// and brings up USOCK + subscriptions through ReconnectUSock as a side
// effect. If recovery fails for any reason we fall back to normal startup
// and let the service run in degraded mode.
//
// Otherwise (probe times out or returns garbage) the device is presumed to
// be running an application; we open USOCK, subscribe to Redis, and run the
// nRF init sequence. The probe penalty when no recovery is needed is
// acceptable for a service that lives forever.
func (s *Service) Bootstrap() error {
	const probeTimeout = 500 * time.Millisecond

	s.mu.RLock()
	device := s.serialDevice
	baud := s.baudRate
	fu := s.firmwareUpdater
	s.mu.RUnlock()

	if device == "" {
		return fmt.Errorf("Bootstrap: serial config not set; call SetSerialConfig first")
	}

	if info, err := QueryDFUFirmwareVersion(device, baud, nrfDFUImageIdxBootloader, probeTimeout); err == nil {
		s.log.Warnf("nRF found in DFU mode at startup (BL version=%d), running recovery", info.Version)
		if fu == nil {
			s.log.Errorf("Firmware updater not configured; cannot recover, falling back to normal startup")
		} else if err := fu.RecoverFromDFU(); err == nil {
			s.log.Infof("DFU recovery succeeded; service is up via recovery's reconnect path")
			return nil
		} else {
			s.log.Errorf("DFU recovery failed: %v. Falling back to normal startup.", err)
		}
	} else {
		s.log.Debugf("BL probe found no DFU at startup (likely running app): %v", err)
	}

	return s.startNormally()
}

// startNormally is the non-recovery half of Bootstrap: opens USOCK, sets the
// error handler, clears the serial-port fault, subscribes to Redis channels,
// and runs the nRF init sequence. Sets the FaultNRFInit fault on init
// failure rather than returning an error, since a bad init shouldn't prevent
// the rest of the service from running.
func (s *Service) startNormally() error {
	s.mu.RLock()
	device := s.serialDevice
	baud := s.baudRate
	handler := s.usockHandler
	errHandler := s.usockErrHandler
	s.mu.RUnlock()

	sock, err := usock.New(device, baud, handler, s.log)
	if err != nil {
		return fmt.Errorf("failed to open USOCK: %w", err)
	}
	if errHandler != nil {
		sock.SetErrorHandler(errHandler)
	}
	sock.SetSyncFrameIDs(ble.FrameOTAData, ble.FrameOTACtrl)
	s.SetUSock(sock)
	s.ClearFault(FaultSerialPort)
	s.log.Infof("Connected to nRF52 via USOCK")

	s.link.resetToDefault()

	s.SubscribeToRedisChannels()

	s.log.Infof("Initializing communication with nRF52...")
	if err := s.InitializeNRF52(); err != nil {
		s.SetFault(FaultNRFInit, fmt.Sprintf("nRF52 initialization sequence failed: %v", err))
		s.log.Errorf("Error during nRF52 initialization sequence: %v", err)
	} else {
		s.ClearFault(FaultNRFInit)
		s.log.Infof("nRF52 initialization sequence sent successfully.")
	}

	s.link.StartNegotiation()

	return nil
}

// Wait waits for all service goroutines to finish
func (s *Service) Wait() {
	s.wg.Wait()
}

// StopSubscriptions stops all Redis subscriptions
func (s *Service) StopSubscriptions() {
	s.subMu.Lock()
	if s.subStopCh != nil {
		close(s.subStopCh)
		s.subStopCh = nil
	}
	s.subMu.Unlock()
	s.subWg.Wait()
	s.log.Infof("Redis subscriptions stopped")
}

// RestartSubscriptions stops existing subscriptions and starts new ones
func (s *Service) RestartSubscriptions() {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	s.StopSubscriptions()
	s.SubscribeToRedisChannels()
}

// resyncStateToNRF re-pushes all iMX->nRF state after a baud negotiation.
// The Redis subscriptions run concurrently with the link negotiation, and a
// frame written while the port is being reopened for a baud change is
// silently lost — either written at the old rate to an nRF that has already
// switched, or into the closing fd. A state field that never changes again
// (vehicle "stand-by" after the post-hibernation boot) then stays stale on
// the BLE side indefinitely, so every negotiation that reopened the port is
// followed by a full re-push: clear the dedup caches and restart the
// subscriptions — StartWithSync re-emits every field.
func (s *Service) resyncStateToNRF(reason string) {
	s.log.Infof("Re-pushing state to nRF after %s", reason)

	s.mu.Lock()
	s.lastMileage = ""
	s.lastVehicleState = ""
	s.mu.Unlock()

	s.RestartSubscriptions()
}

// resyncAfterNRFReset re-pushes all iMX->nRF state after the firmware rebooted
// and lost its cache. The serial link is still up (only the nRF reset), so it
// restarts the Redis subscriptions — StartWithSync re-emits every field — and
// re-initializes streaming, rather than reopening USOCK. The dedup caches are
// cleared first so the re-pushes aren't suppressed against values that were
// sent to the pre-reset firmware. Debounced because the nRF re-sends
// RESET_INFO until ACKed.
func (s *Service) resyncAfterNRFReset() {
	s.resyncMu.Lock()
	if time.Since(s.lastResetResync) < 5*time.Second {
		s.resyncMu.Unlock()
		return
	}
	s.lastResetResync = time.Now()
	s.resyncMu.Unlock()

	s.log.Infof("nRF reset: resyncing state to firmware")

	s.mu.Lock()
	s.lastMileage = ""
	s.lastVehicleState = ""
	s.mu.Unlock()

	s.RestartSubscriptions()

	if err := s.InitializeNRF52(); err != nil {
		s.log.Errorf("nRF reset resync: re-initialization failed: %v", err)
	}

	// a reset nRF is back at the boot baud rate with fresh capabilities;
	// renegotiate (no-op for legacy firmware)
	s.link.StartNegotiation()
}

// setErrorAndSleep sets an error state in Redis and blocks forever to prevent restart loops
func (s *Service) setErrorAndSleep(errorMsg string) {
	s.log.Errorf("Fatal error: %s", errorMsg)

	// Write error state to Redis
	if err := s.ipc.Hash("ble").SetMany(map[string]any{
		"service-health": "error",
		"service-error":  errorMsg,
	}); err != nil {
		s.log.Errorf("Failed to write BLE service health: %v", err)
	}

	// Block forever to prevent restart loop
	s.log.Errorf("Blocking forever to prevent restart loop")
	select {} // Block indefinitely
}

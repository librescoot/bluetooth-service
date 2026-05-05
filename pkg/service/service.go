package service

import (
	"fmt"
	"sync"
	"time"

	ipc "github.com/librescoot/redis-ipc"

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
	faults          *ipc.FaultSet // Fault tracking
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

	// Signaled by handleBLEVersionMessage whenever a BLE version frame
	// arrives from the nRF. InitializeNRF52 drains this channel before
	// sending the version request, then waits on it to confirm the
	// handshake completed before returning. Buffered (size 1) so the
	// version handler's send is non-blocking.
	versionRxCh chan struct{}
}

// Fault codes for bluetooth service
const (
	FaultSerialPort     = 1 // Serial port communication error
	FaultNRFInit        = 2 // nRF52 initialization error
	FaultFirmwareUpdate = 3 // Firmware update error
)

// New creates a new Service instance
func New(ipcClient *ipc.Client, log *logger.Logger) *Service {
	return &Service{
		ipc:         ipcClient,
		log:         log,
		stopCh:      make(chan struct{}),
		faults:      ipcClient.NewFaultSet("ble:fault", "ble", "fault"),
		versionRxCh: make(chan struct{}, 1),
	}
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

	sock, err := usock.New(device, baud, handler, s.log)
	if err != nil {
		return fmt.Errorf("failed to reconnect to nRF52: %w", err)
	}
	if errHandler != nil {
		sock.SetErrorHandler(errHandler)
	}

	s.mu.Lock()
	s.usock = sock
	s.mu.Unlock()

	if err := s.InitializeNRF52(); err != nil {
		return fmt.Errorf("failed to re-initialize nRF52: %w", err)
	}

	return nil
}

// SetFault adds a fault code to the fault set
func (s *Service) SetFault(code int) {
	if err := s.faults.Add(code); err != nil {
		s.log.Errorf("Failed to add fault %d: %v", code, err)
	}
}

// ClearFault removes a fault code from the fault set
func (s *Service) ClearFault(code int) {
	if err := s.faults.Remove(code); err != nil {
		s.log.Errorf("Failed to remove fault %d: %v", code, err)
	}
}

// ClearAllFaults removes all fault codes
func (s *Service) ClearAllFaults() {
	if err := s.faults.Clear(); err != nil {
		s.log.Errorf("Failed to clear faults: %v", err)
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
	s.SetUSock(sock)
	s.ClearFault(FaultSerialPort)
	s.log.Infof("Connected to nRF52 via USOCK")

	s.SubscribeToRedisChannels()

	s.log.Infof("Initializing communication with nRF52...")
	if err := s.InitializeNRF52(); err != nil {
		s.SetFault(FaultNRFInit)
		s.log.Errorf("Error during nRF52 initialization sequence: %v", err)
	} else {
		s.ClearFault(FaultNRFInit)
		s.log.Infof("nRF52 initialization sequence sent successfully.")
	}

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
	s.StopSubscriptions()
	s.SubscribeToRedisChannels()
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

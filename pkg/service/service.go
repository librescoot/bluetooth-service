package service

import (
	"fmt"
	"sync"

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
	firmwareUpdater firmwareUpdaterInterface
	autoUpdate      bool
	mu              sync.RWMutex
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
		ipc:    ipcClient,
		log:    log,
		stopCh: make(chan struct{}),
		faults: ipcClient.NewFaultSet("ble:fault", "ble", "fault"),
	}
}

// SetUSock sets the USOCK connection for the service
func (s *Service) SetUSock(sock *usock.USOCK) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usock = sock
}

// SetSerialConfig stores the serial configuration for reconnection
func (s *Service) SetSerialConfig(device string, baud int, handler func(*usock.Payload)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serialDevice = device
	s.baudRate = baud
	s.usockHandler = handler
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

// ReconnectUSock reopens the serial connection and re-initializes the nRF
func (s *Service) ReconnectUSock() error {
	s.mu.Lock()
	if s.serialDevice == "" || s.usockHandler == nil {
		s.mu.Unlock()
		return fmt.Errorf("serial config not set")
	}
	device := s.serialDevice
	baud := s.baudRate
	handler := s.usockHandler
	s.mu.Unlock()

	sock, err := usock.New(device, baud, handler, s.log)
	if err != nil {
		return fmt.Errorf("failed to reconnect to nRF52: %w", err)
	}

	s.mu.Lock()
	s.usock = sock
	s.mu.Unlock()

	// Re-initialize nRF52
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

// Stop stops the service
func (s *Service) Stop() {
	close(s.stopCh)
}

// Wait waits for all service goroutines to finish
func (s *Service) Wait() {
	s.wg.Wait()
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


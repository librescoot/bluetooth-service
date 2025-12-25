package service

import (
	"sync"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// usockWriter is an interface for writing USOCK messages (for testing)
type usockWriter interface {
	WriteWithFrameID(frameID byte, data []byte) error
}

// Service represents the MDB Bluetooth service
type Service struct {
	usock  usockWriter
	ipc    *ipc.Client // Redis IPC client
	log    *logger.Logger
	stopCh chan struct{}
	wg     sync.WaitGroup
	faults *ipc.FaultSet // Fault tracking
}

// Fault codes for bluetooth service
const (
	FaultSerialPort = 1 // Serial port communication error
	FaultNRFInit    = 2 // nRF52 initialization error
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
	s.usock = sock
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


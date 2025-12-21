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

// Service represents the MDB Bluetooth service
type Service struct {
	usock         usockWriter
	ipc           *ipc.Client // Redis IPC client
	log           *logger.Logger
	stopCh        chan struct{}
	wg            sync.WaitGroup
	healthStatus  string // "connected", "disconnected", or "error"
	healthError   string // Error message when status is "error"
	healthMutex   sync.Mutex
	hasSeenFrames bool // Track if we've successfully received frames

}

// New creates a new Service instance
func New(ipcClient *ipc.Client, log *logger.Logger) *Service {
	return &Service{
		ipc:          ipcClient,
		log:          log,
		stopCh:       make(chan struct{}),
		healthStatus: "disconnected", // Start in disconnected state
	}
}

// SetUSock sets the USOCK connection for the service
func (s *Service) SetUSock(sock *usock.USOCK) {
	s.usock = sock
}

// Stop stops the service
func (s *Service) Stop() {
	close(s.stopCh)
}

// Wait waits for all service goroutines to finish
func (s *Service) Wait() {
	s.wg.Wait()
}

// SetBLEError sets the BLE service health to error and writes to Redis
func (s *Service) SetBLEError(errorMsg string) {
	s.healthMutex.Lock()
	defer s.healthMutex.Unlock()

	// Only update if the error status actually changed
	if s.healthStatus == "error" && s.healthError == errorMsg {
		return
	}

	s.healthStatus = "error"
	s.healthError = errorMsg

	s.log.Warnf("Setting BLE service health to error: %s", errorMsg)

	// Write to Redis
	if err := s.ipc.Hash("ble").SetMany(map[string]any{
		"service-health": "error",
		"service-error":  errorMsg,
	}); err != nil {
		s.log.Errorf("Failed to write BLE service health to Redis: %v", err)
	}
}

// ClearBLEError clears the BLE service health error status
func (s *Service) ClearBLEError() {
	s.healthMutex.Lock()
	defer s.healthMutex.Unlock()

	// Only update if we're transitioning from error state
	if s.healthStatus != "error" {
		return
	}

	s.log.Infof("Clearing BLE service health error")

	// Restore to normal (non-error) state
	s.healthStatus = "ok"
	s.healthError = ""

	if err := s.ipc.Hash("ble").Set("service-health", "ok"); err != nil {
		s.log.Errorf("Failed to write BLE service-health to Redis: %v", err)
	}
	if err := s.ipc.Hash("ble").Delete("service-error"); err != nil {
		s.log.Errorf("Failed to delete BLE service-error field: %v", err)
	}
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

// StartHealthHeartbeat starts a goroutine that updates the last-update timestamp every 5 seconds
func (s *Service) StartHealthHeartbeat() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		s.log.Debugf("Starting BLE health heartbeat")

		// Initialize service-health to "ok" when heartbeat starts
		s.healthMutex.Lock()
		if s.healthStatus != "error" {
			s.healthStatus = "ok"
			if err := s.ipc.Hash("ble").Set("service-health", "ok"); err != nil {
				s.log.Errorf("Failed to initialize BLE service-health: %v", err)
			}
		}
		s.healthMutex.Unlock()

		for {
			select {
			case <-s.stopCh:
				s.log.Debugf("Stopping BLE health heartbeat")
				return
			case <-ticker.C:
				timestamp := time.Now().Unix()
				if err := s.ipc.Hash("ble").Set("last-update", fmt.Sprintf("%d", timestamp)); err != nil {
					s.log.Errorf("Failed to update BLE heartbeat timestamp: %v", err)
				} else {
					s.log.Debugf("Updated BLE heartbeat timestamp: %d", timestamp)
				}
			}
		}
	}()
}

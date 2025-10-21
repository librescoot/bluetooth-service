package service

import (
	"sync"

	"github.com/librescoot/bluetooth-service/pkg/logger"
	redisclient "github.com/librescoot/bluetooth-service/pkg/redis"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// Service represents the MDB Bluetooth service
type Service struct {
	usock               *usock.USOCK
	redis               *redisclient.Client
	log                 *logger.Logger
	fileTransferManager *FileTransferManager
	activeTransferID    string // Track the current active transfer ID
	stopCh              chan struct{}
	wg                  sync.WaitGroup
}

// New creates a new Service instance
func New(redisClient *redisclient.Client, log *logger.Logger) *Service {
	// Initialize file transfer manager
	ftm, err := NewFileTransferManager(log)
	if err != nil {
		log.Errorf("Failed to initialize file transfer manager: %v", err)
		// Continue without file transfer support
		ftm = nil
	}

	return &Service{
		redis:               redisClient,
		log:                 log,
		fileTransferManager: ftm,
		stopCh:              make(chan struct{}),
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

package service

import (
	redisclient "github.com/librescoot/bluetooth-service/pkg/redis"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// Service represents the MDB Bluetooth service
type Service struct {
	usock  *usock.USOCK
	redis  *redisclient.Client
	stopCh chan struct{}
}

// New creates a new Service instance
func New(redisClient *redisclient.Client) *Service {
	return &Service{
		redis:  redisClient,
		stopCh: make(chan struct{}),
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

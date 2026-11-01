package service

import (
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

const testNow int64 = 1735689600 // 2025-01-01T00:00:00Z

func newNRFTimeTestService() (*Service, *mockUSOCK) {
	mock := &mockUSOCK{}
	return &Service{
		usock:      mock,
		log:        logger.NewLogger(nil, logger.LogLevelNone),
		nrfTimeNow: func() time.Time { return time.Unix(testNow, 0) },
	}, mock
}

func TestLinkCapsWriteFailureIsUnavailable(t *testing.T) {
	s, mock := newNRFTimeTestService()
	s.link = newLinkManager(s)
	mock.writeError = errors.New("closed descriptor")

	caps, ok, err := s.link.probeCaps()
	if caps != 0 || ok {
		t.Fatalf("probeCaps() = (%d, %v, %v), want no capabilities", caps, ok, err)
	}
	if !errors.Is(err, errLinkUnavailable) {
		t.Fatalf("probeCaps() error = %v, want errLinkUnavailable", err)
	}
}

func TestStopPermanentlyDisablesLinkManager(t *testing.T) {
	s, _ := newNRFTimeTestService()
	s.stopCh = make(chan struct{})
	s.link = newLinkManager(s)

	s.Stop()

	s.link.mu.Lock()
	suspended := s.link.suspended
	stopped := s.link.stopped
	s.link.mu.Unlock()
	if !suspended || !stopped {
		t.Fatal("Stop() left link manager active")
	}
	if s.link.Resume() {
		t.Fatal("Resume() reactivated a stopped link manager")
	}
	if s.link.beginOp() {
		t.Fatal("beginOp() acquired a stopped link manager")
	}
	if err := s.ReconnectUSock(); err == nil {
		t.Fatal("ReconnectUSock() reactivated a stopped service")
	}
}

func TestShutdownWaitsForReconnectBeforeDisablingStream(t *testing.T) {
	s, mock := newNRFTimeTestService()
	s.link = newLinkManager(s)
	s.reconnectMu.Lock()

	done := make(chan struct{})
	go func() {
		s.ShutdownNRF52()
		close(done)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		s.mu.RLock()
		serviceStopped := s.stopped
		s.mu.RUnlock()
		if serviceStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not establish terminal service state")
		}
		time.Sleep(time.Millisecond)
	}

	select {
	case <-done:
		t.Fatal("shutdown completed while reconnect still owned the lifecycle")
	default:
	}
	if len(mock.messages) != 0 {
		t.Fatal("shutdown disabled streaming before reconnect completed")
	}

	s.reconnectMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not resume after reconnect completed")
	}
	if len(mock.messages) != 1 {
		t.Fatalf("shutdown wrote %d messages, want stream disable", len(mock.messages))
	}
	s.link.mu.Lock()
	linkStopped := s.link.stopped
	linkSuspended := s.link.suspended
	s.link.mu.Unlock()
	if !linkStopped || !linkSuspended {
		t.Fatal("shutdown left link manager active")
	}
}

func rtcPayload(t *testing.T, messageType ble.MessageType, key uint16, value interface{}) *usock.Payload {
	t.Helper()
	data, err := cbor.Marshal(map[uint16]map[uint16]interface{}{
		uint16(messageType): {key: value},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &usock.Payload{ID: byte(messageType), Data: data, Size: len(data)}
}

func rtcReplyPayload(t *testing.T, token uint64, value interface{}) *usock.Payload {
	t.Helper()
	data, err := cbor.Marshal(map[uint16]map[uint16]interface{}{
		uint16(ble.TypeRTC): {
			uint16(ble.TypeRTC) + uint16(ble.TypeRTCGet):   value,
			uint16(ble.TypeRTC) + uint16(ble.TypeRTCToken): token,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &usock.Payload{ID: byte(uint16(ble.TypeRTC) & 0xff), Data: data, Size: len(data)}
}

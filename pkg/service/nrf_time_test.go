package service

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

const testNow int64 = 1735689600 // 2025-01-01T00:00:00Z

func newNRFTimeTestService(setter func(int64) error) (*Service, *mockUSOCK) {
	mock := &mockUSOCK{}
	return &Service{
		usock:       mock,
		log:         logger.NewLogger(nil, logger.LogLevelNone),
		nrfTimeNow:  func() time.Time { return time.Unix(testNow, 0) },
		clockSetter: setter,
	}, mock
}

func armNRFTime(s *Service, generation, token uint64) {
	s.nrfTimeMu.Lock()
	s.nrfTimeState = nrfTimeState{pending: true, generation: generation, token: token}
	s.nrfTimeMu.Unlock()
}

func TestLinkCapsWriteFailureIsUnavailable(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
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
	s, _ := newNRFTimeTestService(nil)
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
	s, mock := newNRFTimeTestService(nil)
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
		s.link.mu.Lock()
		linkStopped := s.link.stopped
		s.link.mu.Unlock()
		if serviceStopped && linkStopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown did not establish terminal state")
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

func TestNRFTimeRequestCodec(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	s.requestNRFTime()
	if mock.messageCount() != 1 {
		t.Fatalf("messages = %d, want 1", mock.messageCount())
	}
	msg := mock.lastMessage()
	if msg.frameID != byte(uint16(ble.TypeRTC)&0xff) {
		t.Errorf("frame ID = %#x, want %#x", msg.frameID, byte(uint16(ble.TypeRTC)&0xff))
	}
	var decoded map[uint16]map[uint16]interface{}
	if err := cbor.Unmarshal(msg.data, &decoded); err != nil {
		t.Fatal(err)
	}
	if value := decoded[uint16(ble.TypeRTC)][uint16(ble.TypeRTC)+uint16(ble.TypeRTCGet)]; value != nil {
		t.Errorf("GET value = %#v, want null", value)
	}
	if token, ok := decoded[uint16(ble.TypeRTC)][uint16(ble.TypeRTC)+uint16(ble.TypeRTCToken)].(uint64); !ok || token == 0 {
		t.Errorf("GET token = %#v, want non-zero uint64", token)
	}
}

func TestNRFTimeRequestWriteFailureClearsPendingState(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	mock.writeError = errors.New("write failed")
	s.requestNRFTime()
	s.nrfTimeMu.Lock()
	state := s.nrfTimeState
	s.nrfTimeMu.Unlock()
	if state.pending || state.token != 0 {
		t.Fatalf("state after write failure = %+v, want no pending token", state)
	}
}

func TestNRFTimeValidNewerSampleAdvances(t *testing.T) {
	var got []int64
	s, _ := newNRFTimeTestService(func(epoch int64) error { got = append(got, epoch); return nil })
	armNRFTime(s, 1, 1)
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(testNow+60)))
	if len(got) != 1 || got[0] != testNow+60 {
		t.Errorf("setter calls = %v, want [%d]", got, testNow+60)
	}
}

func TestNRFTimeRejectsRollbackAndImplausibleSamples(t *testing.T) {
	now := time.Unix(testNow, 0)
	cases := []interface{}{uint64(testNow), uint64(testNow - 1), uint64(nrfTimePlausibility - 1), uint64(now.AddDate(nrfTimeMaximumHoldoverYears, 0, 0).Add(time.Second).Unix()), int64(-1), nil}
	for _, sample := range cases {
		t.Run("sample", func(t *testing.T) {
			calls := 0
			s, _ := newNRFTimeTestService(func(int64) error { calls++; return nil })
			armNRFTime(s, 1, 1)
			s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, sample))
			if calls != 0 {
				t.Errorf("setter called for rejected sample %#v", sample)
			}
		})
	}
}

func TestNRFTimeAcceptsLongParkedHoldover(t *testing.T) {
	now := time.Unix(testNow, 0)
	for _, sample := range []time.Time{now.AddDate(0, 0, 30), now.AddDate(1, 0, 0)} {
		t.Run(sample.Format("2006-01-02"), func(t *testing.T) {
			calls := 0
			s, _ := newNRFTimeTestService(func(int64) error { calls++; return nil })
			armNRFTime(s, 1, 1)
			s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(sample.Unix())))
			if calls != 1 {
				t.Fatalf("setter calls = %d, want 1", calls)
			}
		})
	}
}

func TestNRFTimeMaximumHoldoverBound(t *testing.T) {
	now := time.Unix(testNow, 0)
	maximum := now.AddDate(nrfTimeMaximumHoldoverYears, 0, 0)

	calls := 0
	s, _ := newNRFTimeTestService(func(int64) error { calls++; return nil })
	armNRFTime(s, 1, 1)
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(maximum.Unix())))
	if calls != 1 {
		t.Fatalf("setter calls at maximum holdover = %d, want 1", calls)
	}

	calls = 0
	s, _ = newNRFTimeTestService(func(int64) error { calls++; return nil })
	armNRFTime(s, 1, 1)
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(maximum.Add(time.Second).Unix())))
	if calls != 0 {
		t.Fatalf("setter calls beyond maximum holdover = %d, want 0", calls)
	}
}

func TestNRFTimeIgnoresUnsolicitedDuplicateAndWrongRoute(t *testing.T) {
	calls := 0
	s, _ := newNRFTimeTestService(func(int64) error { calls++; return nil })
	armNRFTime(s, 1, 1)
	s.HandleUSockMessage(byte(uint16(ble.TypeScooterInfo)&0xff), rtcPayload(t, ble.TypeScooterInfo, uint16(ble.TypeScooterInfo)+uint16(ble.TypeSystemTime), uint64(testNow+60)))
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(testNow+60)))
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(testNow+120)))
	if calls != 1 {
		t.Errorf("setter calls = %d, want 1", calls)
	}
}

func TestNRFTimeRejectsStaleReplyAfterNewRequest(t *testing.T) {
	calls := 0
	s, _ := newNRFTimeTestService(func(int64) error { calls++; return nil })
	armNRFTime(s, 1, 11)
	armNRFTime(s, 2, 22)
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 11, uint64(testNow+60)))
	if calls != 0 {
		t.Fatalf("stale reply setter calls = %d, want 0", calls)
	}
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 22, uint64(testNow+60)))
	if calls != 1 {
		t.Fatalf("current reply setter calls = %d, want 1", calls)
	}
}

func TestNRFTimeLegacyTimeoutDoesNotClearNewerRequest(t *testing.T) {
	s, _ := newNRFTimeTestService(nil)
	armNRFTime(s, 2, 2)
	s.expireNRFTimeRequest(1)
	s.nrfTimeMu.Lock()
	pending := s.nrfTimeState.pending
	s.nrfTimeMu.Unlock()
	if !pending {
		t.Error("stale timeout cleared current request")
	}
	s.expireNRFTimeRequest(2)
	s.nrfTimeMu.Lock()
	pending = s.nrfTimeState.pending
	s.nrfTimeMu.Unlock()
	if pending {
		t.Error("current timeout did not clear request")
	}
}

func TestNRFTimeSetFailureDoesNotAdvance(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	s, _ := newNRFTimeTestService(func(int64) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return errors.New("denied")
	})
	armNRFTime(s, 1, 1)
	s.HandleUSockMessage(byte(uint16(ble.TypeRTC)&0xff), rtcReplyPayload(t, 1, uint64(testNow+60)))
	if calls != 1 {
		t.Errorf("setter calls = %d, want 1", calls)
	}
}

func TestNRFTimeSeedRequiresExplicitTrustedClock(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	s.seedNRFTime(testNow, "test")
	if mock.messageCount() != 1 {
		t.Fatalf("seed writes = %d, want 1", mock.messageCount())
	}
	var decoded map[uint16]map[uint16]uint64
	if err := cbor.Unmarshal(mock.lastMessage().data, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded[uint16(ble.TypeRTC)][uint16(ble.TypeRTC)+uint16(ble.TypeRTCSet)]; got != uint64(testNow) {
		t.Errorf("seed value = %d, want %d", got, testNow)
	}
}

func TestTrustedTimeSeedsOnNTPTransitions(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	trusted := false
	s.trustedTimeNTP = func() bool { return trusted }
	s.trustedTimeGPS = func() string { return "" }

	s.syncTrustedTime()
	trusted = true
	s.syncTrustedTime()
	s.syncTrustedTime()
	trusted = false
	s.syncTrustedTime()
	trusted = true
	s.syncTrustedTime()

	if got := mock.messageCount(); got != 2 {
		t.Errorf("NTP seed writes = %d, want 2", got)
	}
}

func TestTrustedTimeRetriesNTPSeedAfterWriteFailure(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	s.trustedTimeNTP = func() bool { return true }
	s.trustedTimeGPS = func() string { return "" }
	mock.writeError = errors.New("link unavailable")
	s.syncTrustedTime()
	mock.writeError = nil
	s.syncTrustedTime()

	if got := mock.messageCount(); got != 1 {
		t.Errorf("NTP retry writes = %d, want 1", got)
	}
}

func TestTrustedTimeSeedsForEachGPSConfirmation(t *testing.T) {
	s, mock := newNRFTimeTestService(nil)
	gpsSync := ""
	s.trustedTimeNTP = func() bool { return false }
	s.trustedTimeGPS = func() string { return gpsSync }

	s.syncTrustedTime()
	gpsSync = "2026-01-01T00:00:00Z"
	s.syncTrustedTime()
	s.syncTrustedTime()
	gpsSync = "2026-01-01T00:01:00Z"
	s.syncTrustedTime()

	if got := mock.messageCount(); got != 2 {
		t.Errorf("GPS seed writes = %d, want 2", got)
	}
}

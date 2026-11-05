package service

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

func TestHibernationLEDSettings(t *testing.T) {
	for _, general := range []string{"", "false", "true", "invalid"} {
		for _, hibernate := range []string{"", "false", "true", "invalid"} {
			for _, alarm := range []string{"", "armed", "disarmed", "level-2-triggered"} {
				s, mr := newVersionPushService(t)
				s.link = &LinkManager{caps: ble.LinkCapHibernationLED}
				if general != "" {
					mr.HSet("settings", alarmLEDSetting, general)
				}
				if hibernate != "" {
					mr.HSet("settings", alarmLEDHibernateSetting, hibernate)
				}
				if alarm != "" {
					mr.HSet("alarm", "status", alarm)
				}
				if err := s.syncHibernationLED(); err != nil {
					t.Fatal(err)
				}
				msg, err := decodeCBORMessage(s.usock.(*mockUSOCK).lastMessage().data)
				if err != nil {
					t.Fatal(err)
				}
				got := msg[uint16(ble.TypePowerManagement)][uint16(ble.TypePowerManagement)+uint16(ble.TypePowerManagementHibernationLED)]
				want := uint16(0)
				if general == "true" && (hibernate == "" || hibernate == "true") && alarm == "armed" {
					want = 1
				}
				if got != want {
					t.Fatalf("general=%q hibernate=%q alarm=%q: got %d want %d", general, hibernate, alarm, got, want)
				}
			}
		}
	}
}

func TestHibernationLEDRequiresCapability(t *testing.T) {
	s, _ := newVersionPushService(t)
	for _, link := range []*LinkManager{nil, {caps: ble.LinkCapBaud1M | ble.LinkCapOTATunnel | 0x04}} {
		s.link = link
		if err := s.syncHibernationLED(); err != nil {
			t.Fatal(err)
		}
	}
	if s.usock.(*mockUSOCK).messageCount() != 0 {
		t.Fatal("sent retention command to unsupported firmware")
	}
}

func TestHibernationLEDReadFailureDisablesRetention(t *testing.T) {
	s, _ := newVersionPushService(t)
	s.link = &LinkManager{caps: ble.LinkCapHibernationLED}
	s.ipc.Raw().Close()
	if err := s.syncHibernationLED(); err == nil {
		t.Fatal("expected Redis error")
	}
	msg, err := decodeCBORMessage(s.usock.(*mockUSOCK).lastMessage().data)
	if err != nil {
		t.Fatal(err)
	}
	if msg[0x0800][0x0806] != 0 {
		t.Fatal("retention enabled without readable settings")
	}
}

func TestHibernationLEDWriteFailureIsReturned(t *testing.T) {
	s, _ := newVersionPushService(t)
	s.link = &LinkManager{caps: ble.LinkCapHibernationLED}
	want := errors.New("serial disconnected")
	s.usock.(*mockUSOCK).writeError = want
	if err := s.syncHibernationLED(); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestHibernationLEDRefreshedBeforeShutdown(t *testing.T) {
	s, mr := newVersionPushService(t)
	s.link = &LinkManager{caps: ble.LinkCapHibernationLED}
	mr.HSet("settings", alarmLEDSetting, "true")
	mr.HSet("alarm", "status", "armed")
	if err := s.UpdatePowerManagementState("hibernating-for"); err != nil {
		t.Fatal(err)
	}
	msg, err := decodeCBORMessage(s.usock.(*mockUSOCK).messages[0].data)
	if err != nil {
		t.Fatal(err)
	}
	if msg[0x0800][0x0806] != 1 {
		t.Fatal("retention was not sent before shutdown")
	}
	mr.HSet("settings", alarmLEDHibernateSetting, "false")
	start := s.usock.(*mockUSOCK).messageCount()
	if err := s.UpdatePowerManagementState("hibernating-for"); err != nil {
		t.Fatal(err)
	}
	msg, err = decodeCBORMessage(s.usock.(*mockUSOCK).messages[start].data)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := msg[0x0800][0x0806]
	if !ok || value != 0 {
		t.Fatal("stale retention survived a changed setting")
	}
}

type hibernationLEDRecorder struct {
	mu     sync.Mutex
	values []uint16
}

func (r *hibernationLEDRecorder) WriteWithFrameID(_ byte, data []byte) error {
	msg, err := decodeCBORMessage(data)
	if err != nil {
		return nil
	}
	if value, ok := msg[0x0800][0x0806]; ok {
		r.mu.Lock()
		r.values = append(r.values, value)
		r.mu.Unlock()
	}
	return nil
}
func (r *hibernationLEDRecorder) Close() error { return nil }

func TestHibernationLEDWatcherAndResubscribe(t *testing.T) {
	s, mr := newVersionPushService(t)
	s.link = &LinkManager{caps: ble.LinkCapHibernationLED}
	r := &hibernationLEDRecorder{}
	s.usock = r
	mr.HSet("settings", alarmLEDSetting, "true")
	mr.HSet("alarm", "status", "armed")
	s.SubscribeToRedisChannels()
	t.Cleanup(s.StopSubscriptions)
	wait := func(want uint16) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			r.mu.Lock()
			ok := len(r.values) > 0 && r.values[len(r.values)-1] == want
			r.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("retention did not become %d", want)
	}
	wait(1)
	mr.HSet("settings", alarmLEDHibernateSetting, "false")
	mr.Publish("settings", alarmLEDHibernateSetting)
	wait(0)
	mr.HSet("settings", alarmLEDHibernateSetting, "true")
	mr.Publish("settings", alarmLEDHibernateSetting)
	wait(1)
	mr.HSet("alarm", "status", "disarmed")
	mr.Publish("alarm", "status")
	wait(0)
	s.StopSubscriptions()
	mr.HSet("alarm", "status", "armed")
	r.mu.Lock()
	r.values = nil
	r.mu.Unlock()
	s.SubscribeToRedisChannels()
	wait(1)
}

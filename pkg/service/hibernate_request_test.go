package service

import (
	"reflect"
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

func TestHibernationRequestsAlwaysUsePowerCoordinator(t *testing.T) {
	for _, state := range []string{"parked", "stand-by", "ready-to-drive"} {
		for _, kind := range []ble.HibernationRequest{ble.HibernationRequestManual, ble.HibernationRequestAutomatic} {
			s, mr := newVersionPushService(t)
			mr.HSet("vehicle", "state", state)
			s.handlePowerManagementMessage(ble.TypePowerManagementHibernationRequest, int(kind))
			want := "hibernate-auto"
			if kind == ble.HibernationRequestManual {
				want = "hibernate-manual"
			}
			encoded, err := s.ipc.Codec().Encode(want)
			if err != nil {
				t.Fatal(err)
			}
			commands, err := mr.List("scooter:power")
			if err != nil || !reflect.DeepEqual(commands, []string{string(encoded)}) {
				t.Fatalf("state=%s kind=%d commands=%v err=%v", state, kind, commands, err)
			}
			if mr.Exists("scooter:state") {
				t.Fatal("Bluetooth bypassed PM vehicle preparation")
			}
		}
	}
}

func TestWakeTimerAckPublishesEchoedDuration(t *testing.T) {
	s, mr := newVersionPushService(t)
	for _, ack := range []int{28800, 0, 3600} {
		s.handlePowerManagementMessage(ble.TypePowerManagementWakeTimerSet, ack)
		wantArmed := "false"
		if ack > 0 {
			wantArmed = "true"
		}
		if mr.HGet("power-manager", "wake-timer-armed") != wantArmed {
			t.Fatal("legacy armed telemetry changed")
		}
		stored, err := s.ipc.HGet("power-manager", "wake-timer-ack-seconds")
		if err != nil || stored != map[int]string{28800: "28800", 0: "0", 3600: "3600"}[ack] {
			t.Fatalf("ACK duration=%q err=%v", stored, err)
		}
	}
}

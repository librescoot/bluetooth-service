package service

import (
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

func batteryResponse(t *testing.T, s *Service) string {
	t.Helper()
	messages := s.usock.(*mockUSOCK).messages
	if len(messages) != 1 {
		t.Fatalf("got %d responses", len(messages))
	}
	decoded, err := decodeCBORMessageString(messages[0].data)
	if err != nil {
		t.Fatal(err)
	}
	return decoded[uint16(ble.TypeExtended)][uint16(ble.TypeExtended)+uint16(ble.TypeExtendedResponse)]
}

func TestBatteryIdentitySnapshot(t *testing.T) {
	for _, tc := range []struct {
		name, present, serial, command, want string
	}{
		{"present", "true", "T-UNU22205090007", "snapshot:0", `battery:snapshot:0:{"present":true,"serial_number":"T-UNU22205090007"}`},
		{"absent does not leak retained serial", "false", "T-UNU22205090007", "snapshot:0", `battery:snapshot:0:{"present":false}`},
		{"unknown presence", "", "T-UNU22205090007", "snapshot:0", "battery:error:unavailable"},
		{"placeholder", "true", "unknown", "snapshot:0", "battery:error:invalid-identity"},
		{"canonical hex preserves zeroes", "true", "00000000000000ab\x00", "snapshot:0", `battery:snapshot:0:{"present":true,"serial_number":"00000000000000AB"}`},
		{"invalid slot", "true", "T-UNU22205090007", "snapshot:2", "battery:error:unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, mr := newVersionPushService(t)
			mr.HSet("battery:0", "present", tc.present, "serial-number", tc.serial)
			s.handleBatteryCommand(tc.command)
			if got := batteryResponse(t, s); got != tc.want {
				t.Fatalf("response = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBatterySerialValidation(t *testing.T) {
	for _, serial := range []string{"", "unknown", "unbekannt", "null", "---", "0000", "n/a", "has space", "\u0080", "\u00a0ABC"} {
		if got := canonicalBatterySerial(serial); got != "" {
			t.Errorf("accepted invalid serial %q", serial)
		}
	}
	if got := canonicalBatterySerial("0012345"); got != "0012345" {
		t.Errorf("decimal serial changed: %q", got)
	}
}

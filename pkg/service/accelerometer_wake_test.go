package service

import (
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

func TestAccelerometerWakePayload(t *testing.T) {
	tests := []struct {
		name      string
		subType   ble.SubType
		value     interface{}
		payload   string
		fromQuirk bool
		ok        bool
	}{
		{
			name:    "well-formed suspend wake",
			subType: ble.TypeAccelerometerWakeUpSuspend,
			value:   uint64(1234567890),
			payload: "wake-suspend",
			ok:      true,
		},
		{
			name:    "well-formed hibernation wake",
			subType: ble.TypeAccelerometerWakeUpHibernation,
			value:   uint64(0),
			payload: "wake-hibernation",
			ok:      true,
		},
		{
			// Legacy encoding observed on nRF52 firmware <= v2.3.0-ls: outer
			// id and inner key both 0x0200 (so we see relative subtype 0)
			// with the intended subtype constant 0x0202 in the value field.
			// See librescoot-0vb4.
			name:      "quirk: subtype 0, value=0x0202 decoded as hibernation wake",
			subType:   0,
			value:     uint64(0x0202),
			payload:   "wake-hibernation",
			fromQuirk: true,
			ok:        true,
		},
		{
			name:      "quirk: subtype 0, value=0x0201 decoded as suspend wake",
			subType:   0,
			value:     uint64(0x0201),
			payload:   "wake-suspend",
			fromQuirk: true,
			ok:        true,
		},
		{
			// Same quirk path but with the value arriving as int (depends on the
			// CBOR decoder picking a signed type). convertToInt should normalize.
			name:      "quirk: subtype 0, value as int still decoded",
			subType:   0,
			value:     int(0x0202),
			payload:   "wake-hibernation",
			fromQuirk: true,
			ok:        true,
		},
		{
			name: "subtype 0 with unrecognized value is dropped",
			// Future-proofing: the bandage must not swallow other firmware bugs
			// that happen to also send subtype 0.
			subType: 0,
			value:   uint64(0x9999),
			ok:      false,
		},
		{
			name:    "subtype 0 with non-numeric value is dropped",
			subType: 0,
			value:   "garbage",
			ok:      false,
		},
		{
			name:    "subtype 0 with nil value is dropped",
			subType: 0,
			value:   nil,
			ok:      false,
		},
		{
			name:    "unknown non-zero subtype is dropped",
			subType: ble.SubType(7),
			value:   uint64(0x0202),
			ok:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, fromQuirk, ok := accelerometerWakePayload(tt.subType, tt.value)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if payload != tt.payload {
				t.Errorf("payload = %q, want %q", payload, tt.payload)
			}
			if fromQuirk != tt.fromQuirk {
				t.Errorf("fromQuirk = %v, want %v", fromQuirk, tt.fromQuirk)
			}
		})
	}
}

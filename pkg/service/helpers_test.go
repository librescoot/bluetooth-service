package service

import "testing"

func TestBatteryStateToInt(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		expected int
	}{
		{"unknown state", "unknown", BatteryStateUnknown},
		{"asleep state", "asleep", BatteryStateAsleep},
		{"idle state", "idle", BatteryStateIdle},
		{"active state", "active", BatteryStateActive},
		{"empty string defaults to unknown", "", BatteryStateUnknown},
		{"invalid state defaults to unknown", "invalid-state", BatteryStateUnknown},
		{"mixed case defaults to unknown", "Active", BatteryStateUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := batteryStateToInt(tt.state)
			if result != tt.expected {
				t.Errorf("batteryStateToInt(%q) = %d, want %d", tt.state, result, tt.expected)
			}
		})
	}
}

func TestBatteryStateToString(t *testing.T) {
	tests := []struct {
		name     string
		state    int
		expected string
	}{
		{"unknown state code", BatteryStateUnknown, "unknown"},
		{"asleep state code", BatteryStateAsleep, "asleep"},
		{"idle state code", BatteryStateIdle, "idle"},
		{"active state code", BatteryStateActive, "active"},
		{"invalid state code defaults to unknown", 99, "unknown"},
		{"negative state code defaults to unknown", -1, "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := batteryStateToString(tt.state)
			if result != tt.expected {
				t.Errorf("batteryStateToString(%d) = %q, want %q", tt.state, result, tt.expected)
			}
		})
	}
}

func TestVehicleStateToInt(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		expected  int
		expectOk  bool
	}{
		// Stand-by state
		{"stand-by state", "stand-by", 0, true},

		// Parked states (all map to 1)
		{"parked state", "parked", 1, true},
		{"waiting-handlebar state", "waiting-handlebar", 1, true},
		{"waiting-seatbox state", "waiting-seatbox", 1, true},
		{"waiting-hibernation state", "waiting-hibernation", 1, true},
		{"waiting-hibernation-advanced state", "waiting-hibernation-advanced", 1, true},
		{"waiting-hibernation-seatbox state", "waiting-hibernation-seatbox", 1, true},
		{"waiting-hibernation-confirm state", "waiting-hibernation-confirm", 1, true},

		// Hop-on family
		{"hop-on state", "hop-on", 6, true},
		{"hop-on-learning state", "hop-on-learning", 1, true},

		// Ready to drive
		{"ready-to-drive state", "ready-to-drive", 2, true},

		// Shutting down
		{"shutting-down state", "shutting-down", 3, true},

		// Updating
		{"updating state", "updating", 4, true},

		// Unrecognized inputs return ok=false; callers hold previous BLE state.
		{"empty string is not recognized", "", 0, false},
		{"unknown state is not recognized", "unknown-state", 0, false},
		{"mixed case is not recognized", "PARKED", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := vehicleStateToInt(tt.state)
			if ok != tt.expectOk {
				t.Errorf("vehicleStateToInt(%q) ok = %v, want %v", tt.state, ok, tt.expectOk)
			}
			if ok && result != tt.expected {
				t.Errorf("vehicleStateToInt(%q) = %d, want %d", tt.state, result, tt.expected)
			}
		})
	}
}

func TestConvertToInt(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected int
		shouldOk bool
	}{
		{"int value", int(42), 42, true},
		{"int8 value", int8(42), 42, true},
		{"int16 value", int16(42), 42, true},
		{"int32 value", int32(42), 42, true},
		{"int64 value", int64(42), 42, true},
		{"uint value", uint(42), 42, true},
		{"uint8 value", uint8(42), 42, true},
		{"uint16 value", uint16(42), 42, true},
		{"uint32 value", uint32(42), 42, true},
		{"uint64 value", uint64(42), 42, true},
		{"negative int value", int(-42), -42, true},
		{"zero value", int(0), 0, true},
		{"string value fails", "42", 0, false},
		{"float value fails", 42.5, 0, false},
		{"nil value fails", nil, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := convertToInt(tt.value)
			if ok != tt.shouldOk {
				t.Errorf("convertToInt(%v) ok = %v, want %v", tt.value, ok, tt.shouldOk)
			}
			if ok && result != tt.expected {
				t.Errorf("convertToInt(%v) = %d, want %d", tt.value, result, tt.expected)
			}
		})
	}
}

func TestConvertToString(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected string
		shouldOk bool
	}{
		{"string value", "hello", "hello", true},
		{"empty string", "", "", true},
		{"byte slice", []byte("world"), "world", true},
		{"empty byte slice", []byte{}, "", true},
		{"int value fails", 42, "", false},
		{"nil value fails", nil, "", false},
		{"bool value fails", true, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := convertToString(tt.value)
			if ok != tt.shouldOk {
				t.Errorf("convertToString(%v) ok = %v, want %v", tt.value, ok, tt.shouldOk)
			}
			if ok && result != tt.expected {
				t.Errorf("convertToString(%v) = %q, want %q", tt.value, result, tt.expected)
			}
		})
	}
}

func TestConvertToBytes(t *testing.T) {
	tests := []struct {
		name     string
		value    interface{}
		expected []byte
		shouldOk bool
	}{
		{"byte slice", []byte{1, 2, 3}, []byte{1, 2, 3}, true},
		{"empty byte slice", []byte{}, []byte{}, true},
		{"string value fails", "hello", nil, false},
		{"int value fails", 42, nil, false},
		{"nil value fails", nil, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, ok := convertToBytes(tt.value)
			if ok != tt.shouldOk {
				t.Errorf("convertToBytes(%v) ok = %v, want %v", tt.value, ok, tt.shouldOk)
			}
			if ok {
				if len(result) != len(tt.expected) {
					t.Errorf("convertToBytes(%v) length = %d, want %d", tt.value, len(result), len(tt.expected))
				}
				for i := range result {
					if result[i] != tt.expected[i] {
						t.Errorf("convertToBytes(%v)[%d] = %d, want %d", tt.value, i, result[i], tt.expected[i])
						break
					}
				}
			}
		})
	}
}

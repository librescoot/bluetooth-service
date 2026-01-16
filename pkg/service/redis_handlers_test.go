package service

import (
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// mockUSOCK is a mock implementation of usock.USOCK for testing
type mockUSOCK struct {
	messages   []mockMessage
	writeError error
}

type mockMessage struct {
	frameID byte
	data    []byte
}

func (m *mockUSOCK) WriteWithFrameID(frameID byte, data []byte) error {
	if m.writeError != nil {
		return m.writeError
	}
	msg := mockMessage{
		frameID: frameID,
		data:    make([]byte, len(data)),
	}
	copy(msg.data, data)
	m.messages = append(m.messages, msg)
	return nil
}

func (m *mockUSOCK) Close() error {
	return nil
}

func (m *mockUSOCK) lastMessage() *mockMessage {
	if len(m.messages) == 0 {
		return nil
	}
	return &m.messages[len(m.messages)-1]
}

func (m *mockUSOCK) messageCount() int {
	return len(m.messages)
}

// Helper to decode CBOR message with uint16 values
func decodeCBORMessage(data []byte) (map[uint16]map[uint16]uint16, error) {
	var msg map[uint16]map[uint16]uint16
	err := cbor.Unmarshal(data, &msg)
	return msg, err
}

// Helper to decode CBOR message with int32 values
func decodeCBORMessage32(data []byte) (map[uint16]map[uint16]int32, error) {
	var msg map[uint16]map[uint16]int32
	err := cbor.Unmarshal(data, &msg)
	return msg, err
}

// Helper to decode CBOR message with string values
func decodeCBORMessageString(data []byte) (map[uint16]map[uint16]string, error) {
	var msg map[uint16]map[uint16]string
	err := cbor.Unmarshal(data, &msg)
	return msg, err
}

func TestUpdateVehicleState(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		expectedValue uint16
	}{
		{"stand-by state", "stand-by", 0},
		{"parked state", "parked", 1},
		{"ready-to-drive state", "ready-to-drive", 2},
		{"shutting-down state", "shutting-down", 3},
		{"updating state", "updating", 4},
		{"empty string defaults to stand-by", "", 0},
		{"unknown state defaults to stand-by", "unknown", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateVehicleState(tt.state)
			if err != nil {
				t.Fatalf("UpdateVehicleState(%q) returned error: %v", tt.state, err)
			}

			// Verify frame ID
			expectedFrameID := byte(ble.TypeVehicleState & 0xFF)
			if mock.lastMessage().frameID != expectedFrameID {
				t.Errorf("Frame ID = 0x%02x, want 0x%02x", mock.lastMessage().frameID, expectedFrameID)
			}

			// Decode and verify message
			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			// Verify message structure
			if len(msg) != 1 {
				t.Fatalf("Expected 1 message type, got %d", len(msg))
			}

			innerMap := msg[uint16(ble.TypeVehicleState)]
			expectedKey := uint16(ble.TypeVehicleState) + uint16(ble.TypeVehicleStateState)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateSeatboxLock(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		expectedValue uint16
	}{
		{"open state", "open", 1},
		{"closed state", "closed", 0},
		{"numeric 0", "0", 0},
		{"numeric 1", "1", 1},
		{"empty string defaults to 0", "", 0},
		{"invalid string defaults to 0", "invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateSeatboxLock(tt.state)
			if err != nil {
				t.Fatalf("UpdateSeatboxLock(%q) returned error: %v", tt.state, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeVehicleState)]
			expectedKey := uint16(ble.TypeVehicleState) + uint16(ble.TypeVehicleStateSeatbox)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateHandlebarLock(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		expectedValue uint16
	}{
		{"locked state", "locked", 0},
		{"unlocked state", "unlocked", 1},
		{"empty string defaults to locked", "", 0},
		{"invalid string defaults to locked", "invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateHandlebarLock(tt.state)
			if err != nil {
				t.Fatalf("UpdateHandlebarLock(%q) returned error: %v", tt.state, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeVehicleState)]
			expectedKey := uint16(ble.TypeVehicleState) + uint16(ble.TypeVehicleStateHandlebar)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateMileage(t *testing.T) {
	tests := []struct {
		name          string
		mileage       string
		expectedValue int32
	}{
		{"zero mileage", "0", 0},
		{"positive mileage", "12345", 12345},
		{"large mileage", "999999", 999999},
		{"empty string defaults to 0", "", 0},
		{"invalid string defaults to 0", "abc", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateMileage(tt.mileage)
			if err != nil {
				t.Fatalf("UpdateMileage(%q) returned error: %v", tt.mileage, err)
			}

			msg, err := decodeCBORMessage32(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeScooterInfo)]
			expectedKey := uint16(ble.TypeScooterInfo) + uint16(ble.TypeMileage)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateFirmwareVersion(t *testing.T) {
	tests := []struct {
		name     string
		version  string
	}{
		{"semantic version", "1.2.3"},
		{"version with v prefix", "v1.0.0"},
		{"empty version", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateFirmwareVersion(tt.version)
			if err != nil {
				t.Fatalf("UpdateFirmwareVersion(%q) returned error: %v", tt.version, err)
			}

			msg, err := decodeCBORMessageString(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeScooterInfo)]
			expectedKey := uint16(ble.TypeScooterInfo) + uint16(ble.TypeSoftwareVersion)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.version {
				t.Errorf("Value = %q, want %q", val, tt.version)
			}
		})
	}
}

func TestUpdateBatteryActiveStatus(t *testing.T) {
	tests := []struct {
		name          string
		slot          int
		state         string
		expectedValue uint16
		expectedType  ble.SubType
	}{
		{"slot 0 unknown", 0, "unknown", 0, ble.TypeBatterySlot0State},
		{"slot 0 asleep", 0, "asleep", 1, ble.TypeBatterySlot0State},
		{"slot 0 idle", 0, "idle", 2, ble.TypeBatterySlot0State},
		{"slot 0 active", 0, "active", 3, ble.TypeBatterySlot0State},
		{"slot 0 empty defaults to unknown", 0, "", 0, ble.TypeBatterySlot0State},
		{"slot 1 unknown", 1, "unknown", 0, ble.TypeBatterySlot1State},
		{"slot 1 active", 1, "active", 3, ble.TypeBatterySlot1State},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateBatteryActiveStatus(tt.slot, tt.state)
			if err != nil {
				t.Fatalf("UpdateBatteryActiveStatus(%d, %q) returned error: %v", tt.slot, tt.state, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeBattery)]
			expectedKey := uint16(ble.TypeBattery) + uint16(tt.expectedType)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateBatteryPresentStatus(t *testing.T) {
	tests := []struct {
		name          string
		slot          int
		present       string
		expectedValue uint16
		expectedType  ble.SubType
	}{
		{"slot 0 true", 0, "true", 1, ble.TypeBatterySlot0Presence},
		{"slot 0 1", 0, "1", 1, ble.TypeBatterySlot0Presence},
		{"slot 0 false", 0, "false", 0, ble.TypeBatterySlot0Presence},
		{"slot 0 0", 0, "0", 0, ble.TypeBatterySlot0Presence},
		{"slot 0 empty defaults to 0", 0, "", 0, ble.TypeBatterySlot0Presence},
		{"slot 1 true", 1, "true", 1, ble.TypeBatterySlot1Presence},
		{"slot 1 1", 1, "1", 1, ble.TypeBatterySlot1Presence},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateBatteryPresentStatus(tt.slot, tt.present)
			if err != nil {
				t.Fatalf("UpdateBatteryPresentStatus(%d, %q) returned error: %v", tt.slot, tt.present, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeBattery)]
			expectedKey := uint16(ble.TypeBattery) + uint16(tt.expectedType)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateBatteryCycleCount(t *testing.T) {
	tests := []struct {
		name          string
		slot          int
		cycles        string
		expectedValue uint16
		expectedType  ble.SubType
	}{
		{"slot 0 zero cycles", 0, "0", 0, ble.TypeBatterySlot0CycleCount},
		{"slot 0 some cycles", 0, "42", 42, ble.TypeBatterySlot0CycleCount},
		{"slot 0 many cycles", 0, "999", 999, ble.TypeBatterySlot0CycleCount},
		{"slot 0 empty defaults to 0", 0, "", 0, ble.TypeBatterySlot0CycleCount},
		{"slot 1 cycles", 1, "123", 123, ble.TypeBatterySlot1CycleCount},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateBatteryCycleCount(tt.slot, tt.cycles)
			if err != nil {
				t.Fatalf("UpdateBatteryCycleCount(%d, %q) returned error: %v", tt.slot, tt.cycles, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeBattery)]
			expectedKey := uint16(ble.TypeBattery) + uint16(tt.expectedType)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdateBatteryRemainingCharge(t *testing.T) {
	tests := []struct {
		name          string
		slot          int
		charge        string
		expectedValue uint16
		expectedType  ble.SubType
	}{
		{"slot 0 zero charge", 0, "0", 0, ble.TypeBatterySlot0Charge},
		{"slot 0 half charge", 0, "50", 50, ble.TypeBatterySlot0Charge},
		{"slot 0 full charge", 0, "100", 100, ble.TypeBatterySlot0Charge},
		{"slot 0 empty defaults to 0", 0, "", 0, ble.TypeBatterySlot0Charge},
		{"slot 1 charge", 1, "75", 75, ble.TypeBatterySlot1Charge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdateBatteryRemainingCharge(tt.slot, tt.charge)
			if err != nil {
				t.Fatalf("UpdateBatteryRemainingCharge(%d, %q) returned error: %v", tt.slot, tt.charge, err)
			}

			msg, err := decodeCBORMessage(mock.lastMessage().data)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypeBattery)]
			expectedKey := uint16(ble.TypeBattery) + uint16(tt.expectedType)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}
		})
	}
}

func TestUpdatePowerManagementState(t *testing.T) {
	tests := []struct {
		name             string
		state            string
		expectedValue    uint16
		multipleMessages bool // hibernating states send multiple messages
	}{
		{"running state", "running", 1, false},
		{"suspending state", "suspending", 0, false},
		{"hibernating state", "hibernating", 2, true},
		{"hibernating-manual state", "hibernating-manual", 2, true},
		{"hibernating-timer state", "hibernating-timer", 2, true},
		{"hibernating-l2 state", "hibernating-l2", 2, true},
		{"suspending-imminent state", "suspending-imminent", 3, false},
		{"hibernating-imminent state", "hibernating-imminent", 4, false},
		{"hibernating-manual-imminent state", "hibernating-manual-imminent", 4, false},
		{"hibernating-timer-imminent state", "hibernating-timer-imminent", 4, false},
		{"reboot state", "reboot", 5, false},
		{"reboot-imminent state", "reboot-imminent", 1, false},
		{"empty defaults to running", "", 1, false},
		{"unknown defaults to running", "unknown", 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockUSOCK{}
			log := logger.NewLogger(nil, logger.LogLevelNone)
			svc := &Service{usock: mock, log: log}

			err := svc.UpdatePowerManagementState(tt.state)
			if err != nil {
				t.Fatalf("UpdatePowerManagementState(%q) returned error: %v", tt.state, err)
			}

			// For hibernating states, check first message (state message)
			// For other states, check last message (only message)
			var msgData []byte
			if tt.multipleMessages {
				if len(mock.messages) == 0 {
					t.Fatalf("Expected messages but got none")
				}
				msgData = mock.messages[0].data // First message is the state
			} else {
				msgData = mock.lastMessage().data
			}

			msg, err := decodeCBORMessage(msgData)
			if err != nil {
				t.Fatalf("Failed to decode CBOR: %v", err)
			}

			innerMap := msg[uint16(ble.TypePowerManagement)]
			expectedKey := uint16(ble.TypePowerManagement) + uint16(ble.TypePowerManagementState)

			if val, ok := innerMap[expectedKey]; !ok {
				t.Errorf("Key %d not found in message", expectedKey)
			} else if val != tt.expectedValue {
				t.Errorf("Value = %d, want %d", val, tt.expectedValue)
			}

			// Verify hibernating states send multiple messages
			if tt.multipleMessages && mock.messageCount() < 2 {
				t.Errorf("Expected multiple messages for state %q, got %d", tt.state, mock.messageCount())
			}
		})
	}
}

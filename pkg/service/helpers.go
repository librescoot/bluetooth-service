package service

import (
	"fmt"
	"math"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// Convert battery state string to integer
func batteryStateToInt(state string) int {
	switch state {
	case "unknown":
		return BatteryStateUnknown
	case "asleep":
		return BatteryStateAsleep
	case "idle":
		return BatteryStateIdle
	case "active":
		return BatteryStateActive
	default:
		// s.log.Warnf("Unknown battery state: %s, defaulting to Unknown", state)
		return BatteryStateUnknown // Default to unknown
	}
}

// Convert integer battery state to string
func batteryStateToString(state int) string {
	switch state {
	case BatteryStateUnknown:
		return "unknown"
	case BatteryStateAsleep:
		return "asleep"
	case BatteryStateIdle:
		return "idle"
	case BatteryStateActive:
		return "active"
	default:
		// s.log.Warnf("Unknown battery state code: %d", state)
		return "unknown"
	}
}

// Convert string vehicle state to the wire integer the nRF52 expects.
// "hop-on-learning" collapses to PARKED
// because the user is actively interacting with the dashboard (it's only
// "locked-feeling" from the dashboard UI's perspective). "hop-on" gets the
// dedicated value 6 so the firmware picks the right power-rail rules
// (POWER_MODE_ACTIVE) while still presenting BLE clients with the
// "stand-by" state-string (the firmware does that translation locally).
//
// Returns ok=false for unrecognized strings so callers can hold the previous
// BLE state instead of forwarding a misleading default.
func vehicleStateToInt(state string) (int, bool) {
	switch state {
	case "stand-by":
		return 0, true // STANDBY
	case "parked", "hop-on-learning",
		"waiting-handlebar", "waiting-seatbox", "waiting-hibernation",
		"waiting-hibernation-advanced", "waiting-hibernation-seatbox", "waiting-hibernation-confirm":
		return 1, true // PARKED
	case "ready-to-drive":
		return 2, true // READY_TO_DRIVE
	case "shutting-down":
		return 3, true // SHUTTING_DOWN
	case "updating":
		return 4, true // UPDATING
	case "hop-on":
		return 6, true // HOP_ON (firmware presents as stand-by to BLE clients)
	default:
		return 0, false
	}
}

// writeUARTMessage sends a message with an integer value.
// It now calculates the absolute subtype key.
func writeUARTMessage(sock usockWriter, messageType ble.MessageType, subType ble.SubType, value uint16) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}
	absoluteKey := uint16(messageType) + uint16(subType)
	message := map[uint16]map[uint16]uint16{
		uint16(messageType): {
			absoluteKey: value,
		},
	}

	cborData, err := cbor.Marshal(message)
	if err != nil {
		// s.log.Errorf("Failed to marshal CBOR message: %v", err)
		return fmt.Errorf("failed to marshal CBOR: %w", err)
	}

	// Use the lower byte of the MessageType as the Frame ID, matching observed logs.
	frameID := byte(messageType & 0xFF)

	// s.log.Debugf("Sending message: Frame ID=0x%02x, CBOR Data=%s", frameID, hex.EncodeToString(cborData))
	return sock.WriteWithFrameID(frameID, cborData)
}

// writeUARTMessage32 sends a message with a 32-bit integer value.
// This is needed for values that don't fit in uint16, like mileage.
func writeUARTMessage32(sock usockWriter, messageType ble.MessageType, subType ble.SubType, value int32) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}
	absoluteKey := uint16(messageType) + uint16(subType)
	message := map[uint16]map[uint16]int32{
		uint16(messageType): {
			absoluteKey: value,
		},
	}

	cborData, err := cbor.Marshal(message)
	if err != nil {
		// s.log.Errorf("Failed to marshal CBOR message: %v", err)
		return fmt.Errorf("failed to marshal CBOR: %w", err)
	}

	// Use the lower byte of the MessageType as the Frame ID, matching observed logs.
	frameID := byte(messageType & 0xFF)

	// s.log.Debugf("Sending message: Frame ID=0x%02x, CBOR Data=%s", frameID, hex.EncodeToString(cborData))
	return sock.WriteWithFrameID(frameID, cborData)
}

// writeUARTMessageString sends a message with a string value.
// It now calculates the absolute subtype key.
func writeUARTMessageString(sock usockWriter, messageType ble.MessageType, subType ble.SubType, value string) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}
	absoluteKey := uint16(messageType) + uint16(subType)
	message := map[uint16]map[uint16]string{
		uint16(messageType): {
			absoluteKey: value,
		},
	}

	cborData, err := cbor.Marshal(message)
	if err != nil {
		// s.log.Errorf("Failed to marshal CBOR string message: %v", err)
		return fmt.Errorf("failed to marshal CBOR string: %w", err)
	}

	// Use the lower byte of the MessageType as the Frame ID, matching observed logs.
	frameID := byte(messageType & 0xFF)

	// s.log.Debugf("Sending string message: Frame ID=0x%02x, CBOR Data=%s", frameID, hex.EncodeToString(cborData))
	return sock.WriteWithFrameID(frameID, cborData)
}

// Helper function to safely convert interface{} to int
func convertToInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int8:
		return int(v), true
	case int16:
		return int(v), true
	case int32:
		return int(v), true
	case int64:
		if v >= math.MinInt && v <= math.MaxInt {
			return int(v), true
		}
		return 0, false
	case uint:
		if uint64(v) <= math.MaxInt {
			return int(v), true
		}
		return 0, false
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		if uint64(v) <= math.MaxInt {
			return int(v), true
		}
		return 0, false
	case uint64:
		if v <= math.MaxInt {
			return int(v), true
		}
		return 0, false
	default:
		// log.Printf("Value is not a convertible integer type: %T", value)
		return 0, false
	}
}

// Helper function to safely convert interface{} to string
func convertToString(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		// log.Printf("Value is not a string or []byte type: %T", value)
		return "", false
	}
}

// Helper function to safely convert interface{} to byte slice
func convertToBytes(value interface{}) ([]byte, bool) {
	if v, ok := value.([]byte); ok {
		return v, true
	}
	// log.Printf("Value is not a []byte type: %T", value)
	return nil, false
}

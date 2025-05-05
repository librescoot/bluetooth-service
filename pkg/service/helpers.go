package service

import (
	"encoding/hex"
	"fmt"
	"log"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/usock"
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
		log.Printf("Unknown battery state: %s, defaulting to Unknown", state)
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
		log.Printf("Unknown battery state code: %d", state)
		return "unknown"
	}
}

// writeUARTMessage sends a message with an integer value.
// It now calculates the absolute subtype key.
func writeUARTMessage(sock *usock.USOCK, messageType ble.MessageType, subType ble.SubType, value uint16) error {
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
		log.Printf("Failed to marshal CBOR message: %v", err)
		return fmt.Errorf("failed to marshal CBOR: %w", err)
	}

	// Use the lower byte of the MessageType as the Frame ID, matching observed logs.
	frameID := byte(messageType & 0xFF)

	log.Printf("Sending message: Frame ID=0x%02x, CBOR Data=%s", frameID, hex.EncodeToString(cborData))
	return sock.WriteWithFrameID(frameID, cborData)
}

// writeUARTMessageString sends a message with a string value.
// It now calculates the absolute subtype key.
func writeUARTMessageString(sock *usock.USOCK, messageType ble.MessageType, subType ble.SubType, value string) error {
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
		log.Printf("Failed to marshal CBOR string message: %v", err)
		return fmt.Errorf("failed to marshal CBOR string: %w", err)
	}

	// Use the lower byte of the MessageType as the Frame ID, matching observed logs.
	frameID := byte(messageType & 0xFF)

	log.Printf("Sending string message: Frame ID=0x%02x, CBOR Data=%s", frameID, hex.EncodeToString(cborData))
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
		if v <= int64(^uint(0)>>1) && v >= int64(int(^uint(0)>>1)*-1-1) {
			return int(v), true
		}
		log.Printf("Integer value %d out of range for int type", v)
		return 0, false
	case uint:
		if uint64(v) <= uint64(^uint(0)>>1) {
			return int(v), true
		}
		log.Printf("Unsigned integer value %d out of range for int type", v)
		return 0, false
	case uint8:
		return int(v), true
	case uint16:
		return int(v), true
	case uint32:
		if uint64(v) <= uint64(^uint(0)>>1) {
			return int(v), true
		}
		log.Printf("Unsigned integer value %d out of range for int type", v)
		return 0, false
	case uint64:
		if v <= uint64(^uint(0)>>1) {
			return int(v), true
		}
		log.Printf("Unsigned integer value %d out of range for int type", v)
		return 0, false
	default:
		log.Printf("Value is not a convertible integer type: %T", value)
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
		log.Printf("Value is not a string or []byte type: %T", value)
		return "", false
	}
}

// Helper function to safely convert interface{} to byte slice
func convertToBytes(value interface{}) ([]byte, bool) {
	if v, ok := value.([]byte); ok {
		return v, true
	}
	log.Printf("Value is not a []byte type: %T", value)
	return nil, false
}

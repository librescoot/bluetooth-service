package service

import (
	"fmt"
	"log"

	"github.com/fxamacker/cbor/v2"
	"github.com/redis/go-redis/v9"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// HandleUSockMessage handles incoming USOCK messages
func (s *Service) HandleUSockMessage(frameID byte, payload *usock.Payload) {
	var msgData map[uint16]interface{}
	err := cbor.Unmarshal(payload.Data, &msgData)
	if err != nil {
		log.Printf("Failed to decode CBOR message: %v", err)
		log.Printf("Raw payload: %x", payload.Data)
		return
	}

	log.Printf("Received message: Frame ID=0x%02x, Data=%x", frameID, payload.Data)

	var absoluteMsgType uint16
	var params interface{}
	// Find the single top-level key which represents the absolute message type (e.g., 0xA000 for version)
	if len(msgData) != 1 {
		if len(msgData) == 0 && len(payload.Data) == 4 && payload.Data[0] == 0xa1 && payload.Data[2] == frameID && payload.Data[3] == 0xa0 {
			log.Printf("Received acknowledgment (empty map) for Frame ID: 0x%02x", frameID)
			switch frameID {
			case byte(ble.TypeDataStream & 0xFF): // 0xC0
				log.Printf("Received acknowledgment for Data Stream command")
			case byte(ble.TypeBattery & 0xFF): // 0xE0
				log.Printf("Received acknowledgment for Battery command")
			case byte(ble.TypeBLECommand & 0xFF): // 0xAA
				log.Printf("Received acknowledgment for BLE Command")
			case byte(ble.TypeVehicleState & 0xFF): // 0x20 - Also overlaps with TypeBLEDebug
				log.Printf("Received acknowledgment for command with Frame ID 0x20 (Could be Vehicle State or BLE Debug)")
			// Note: Frame ID 0x00 overlaps TypePowerManagement and TypeBLEVersion
			case 0x40:
				log.Printf("Received acknowledgment for command with Frame ID 0x40 (Could be Scooter Info or Aux Battery)")
			case byte(ble.TypeBLEParam & 0xFF): // 0x80
				log.Printf("Received acknowledgment for BLE Param command")
			// Removed specific cases for overlapping Frame IDs (0x00, 0x20, 0x40) that evaluated to the same byte.
			default:
				log.Printf("Received unknown acknowledgment type via Frame ID 0x%02x", frameID)
			}
			return // Processing finished for simple ACK
		} else {
			log.Printf("Received message with unexpected top-level structure (expected 1 key): %d keys", len(msgData))
			return
		}
	}

	for k, v := range msgData {
		absoluteMsgType = k // This is the absolute message type (e.g., 0xA000, 0xA080)
		params = v
		break
	}

	msgType := ble.MessageType(absoluteMsgType)
	log.Printf("Decoded message type: 0x%04x", msgType)

	// Decode the inner parameter map (key should be absolute subtype)
	interMap, okInter := params.(map[interface{}]interface{})
	if !okInter {
		log.Printf("Received message with non-map parameter type: %T", params)
		return
	}

	// Convert inner map keys (likely uint64) to absolute uint16 subtype keys
	paramMap := make(map[uint16]interface{})
	for k, v := range interMap {
		keyInt, okKey := k.(uint64)
		if !okKey {
			log.Printf("Received message with non-uint64 key in parameter map: %T, Key: %v", k, k)
			continue
		}
		if keyInt > 0xFFFF {
			log.Printf("Warning: Received subtype key %d (0x%x) larger than uint16, skipping.", keyInt, keyInt)
			continue
		}
		paramMap[uint16(keyInt)] = v // Key is the ABSOLUTE subtype (e.g., 0xA001)
	}

	// Handle messages based on the ABSOLUTE subtype key found in the inner map
	if len(paramMap) > 0 {
		for absSubTypeKey, value := range paramMap {
			log.Printf("Decoded Absolute Subtype Key: 0x%04x, Value Type: %T", absSubTypeKey, value)

			// Calculate the expected relative subtype for internal logic if needed,
			// but routing should primarily use the absolute key or outer msgType.
			relativeSubType := ble.SubType(0) // Initialize
			// Avoid potential underflow if absSubTypeKey is somehow less than msgType
			if absSubTypeKey >= uint16(msgType) {
				relativeSubType = ble.SubType(absSubTypeKey - uint16(msgType)) // Calculate relative for case matching
			} else {
				log.Printf("Warning: Absolute subtype key 0x%04x is less than message type 0x%04x", absSubTypeKey, msgType)
			}


			switch msgType { // Route based on outer message type
			case ble.TypeBattery:
				// Battery handler needs to determine slot from absolute subtype key
				slot := 0
				// Determine slot based on ABSOLUTE key range from ble/types.go definitions
				if absSubTypeKey >= (uint16(ble.TypeBattery) + uint16(ble.TypeBatterySlot1State)) &&
					absSubTypeKey <= (uint16(ble.TypeBattery) + uint16(ble.TypeBatterySlot1Charge)) { // Simplified range check
					slot = 1
				} else if absSubTypeKey >= (uint16(ble.TypeBattery) + uint16(ble.TypeBatterySlot2State)) &&
					absSubTypeKey <= (uint16(ble.TypeBattery) + uint16(ble.TypeBatterySlot2Charge)) { // Simplified range check
					slot = 2
				}
				if slot != 0 {
					// Pass the RELATIVE subtype to the handler for case matching
					s.handleBatteryMessage(relativeSubType, value, slot)
				} else {
					log.Printf("Could not determine battery slot for absolute subtype key 0x%04x", absSubTypeKey)
				}
			case ble.TypeVehicleState:
				s.handleVehicleStateMessage(relativeSubType, value)
			case ble.TypeScooterInfo:
				s.handleScooterInfoMessage(msgType, absSubTypeKey, value) // Pass absolute subtype key
			case ble.TypeBLEParam, ble.TypeBLEPairingPinDisplay, ble.TypeBLEPairingPinRemove, ble.TypeBLEStatus:
				s.handleBLEParamMessage(msgType, absSubTypeKey, value) // Pass absolute subtype key
			case ble.TypeBLEVersion:
				s.handleBLEVersionMessage(msgType, absSubTypeKey, value) // Pass absolute subtype key
			// Combined BLE Debug handling
			case ble.TypeBLEDebug, ble.TypeBLEReset:
				s.handleBLEDebugMessage(msgType, absSubTypeKey, value) // Pass absolute subtype key
			case ble.TypeDataStream:
				s.handleDataStreamMessage(relativeSubType, value)
			case ble.TypePowerManagement:
				s.handlePowerManagementMessage(relativeSubType, value)
			case ble.TypeAuxBattery:
				s.handleAuxBatteryMessage(relativeSubType, value)
			case ble.TypeBLECommand:
				s.handleBLECommandMessage(msgType, absSubTypeKey, value) // Pass absolute subtype key
			case ble.TypeBatteryInfo: // Add case for Battery Info
				s.handleBatteryInfoMessage(relativeSubType, value) // Call new handler
			case ble.TypePowerMux: // Add case for Power Mux
				s.handlePowerMuxMessage(relativeSubType, value)
			case 0x0000: // Handle generic event messages
				s.handleEventMessage(msgType, absSubTypeKey, value)
			default:
				log.Printf("Unhandled message type: 0x%04x with absolute subtype key 0x%04x", msgType, absSubTypeKey)
			}
		}
	} else {
		log.Printf("Received message type 0x%04x with empty parameter map (might be ACK)", msgType)
		// Handle parameterless messages if needed (e.g., some specific ACKs not caught earlier)
	}
}

// handleBLECommandMessage handles incoming BLE command acknowledgments/responses
func (s *Service) handleBLECommandMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	log.Printf("Handling BLE command message (Type 0x%04x) with absolute subtype key: 0x%04x, value: %v", msgType, absSubTypeKey, value)

	// Determine relative command based on absolute key
	var relativeCmd ble.BLECommand
	if absSubTypeKey >= uint16(ble.TypeBLECommand) && absSubTypeKey < (uint16(ble.TypeBLECommand)+10) { // Rough check
		relativeCmd = ble.BLECommand(absSubTypeKey - uint16(ble.TypeBLECommand))
	} else {
		log.Printf("Warning: Could not determine relative BLE command for absolute key 0x%04x", absSubTypeKey)
		return
	}

	switch relativeCmd {
	case ble.BLECommandAdvStartWithWhitelist:
		log.Printf("Received acknowledgment for Start Advertising (Whitelist) command.")
	case ble.BLECommandAdvRestartNoWhitelist:
		log.Printf("Received acknowledgment for Restart Advertising (No Whitelist) command.")
	case ble.BLECommandAdvStop:
		log.Printf("Received acknowledgment for Stop Advertising command.")
	case ble.BLECommandDeleteBond:
		log.Printf("Received acknowledgment for Delete Bond command.")
	case ble.BLECommandDeleteAllBonds:
		log.Printf("Received acknowledgment for Delete All Bonds command.")
	default:
		log.Printf("Unknown relative BLE command subtype: %d (Absolute Key: 0x%04x)", relativeCmd, absSubTypeKey)
	}
}

// handleBatteryMessage handles battery-related messages
func (s *Service) handleBatteryMessage(subType ble.SubType, value interface{}, slot int) {
	log.Printf("Handling battery message with subtype: %v for slot: %d", subType, slot)
	redisKey := KeyBatterySlot1
	if slot == 2 {
		redisKey = KeyBatterySlot2
	}

	switch subType {
	case ble.TypeBatterySlot1State, ble.TypeBatterySlot2State:
		if state, ok := convertToInt(value); ok {
			log.Printf("Received battery state for slot %d: %d (%s)", slot, state, batteryStateToString(state))
			if err := s.redis.WriteInt(redisKey, "state", state); err != nil {
				log.Printf("Failed to update battery state in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode battery state value: %v", value)
		}

	case ble.TypeBatterySlot1Presence, ble.TypeBatterySlot2Presence:
		if present, ok := convertToInt(value); ok {
			presentBool := present != 0
			log.Printf("Received battery presence for slot %d: %t", slot, presentBool)
			presentStr := "false"
			if presentBool {
				presentStr = "true"
			}
			if err := s.redis.WriteString(redisKey, "present", presentStr); err != nil {
				log.Printf("Failed to update battery presence in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode battery presence value: %v", value)
		}

	case ble.TypeBatterySlot1CycleCount, ble.TypeBatterySlot2CycleCount:
		if count, ok := convertToInt(value); ok {
			log.Printf("Received battery cycle count for slot %d: %d", slot, count)
			if err := s.redis.WriteInt(redisKey, "cycle-count", count); err != nil {
				log.Printf("Failed to update battery cycle count in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode battery cycle count value: %v", value)
		}

	case ble.TypeBatterySlot1Charge, ble.TypeBatterySlot2Charge:
		if charge, ok := convertToInt(value); ok {
			log.Printf("Received battery charge for slot %d: %d %%", slot, charge)
			if err := s.redis.WriteInt(redisKey, "charge", charge); err != nil {
				log.Printf("Failed to update battery charge in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode battery charge value: %v", value)
		}

	default:
		log.Printf("Unknown battery message relative subtype: %v (Absolute Key was processed)", subType)
	}
}

// handleVehicleStateMessage handles vehicle state messages
func (s *Service) handleVehicleStateMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling vehicle state message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeVehicleStateState:
		if state, ok := convertToInt(value); ok {
			log.Printf("Received vehicle state: %d", state)
		} else {
			log.Printf("Could not decode vehicle state value: %v", value)
		}

	case ble.TypeVehicleStateSeatbox:
		if state, ok := convertToInt(value); ok {
			log.Printf("Received seatbox state: %d", state)
		} else {
			log.Printf("Could not decode seatbox state value: %v", value)
		}

	case ble.TypeVehicleStateHandlebar:
		if state, ok := convertToInt(value); ok {
			log.Printf("Received handlebar lock state: %d", state)
		} else {
			log.Printf("Could not decode handlebar lock state value: %v", value)
		}

	default:
		log.Printf("Unknown vehicle state message relative subtype: %v", subType)
	}
}

// handleScooterInfoMessage handles scooter information messages (Mileage, SW Version)
func (s *Service) handleScooterInfoMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	log.Printf("Handling Scooter Info message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	expectedMileageSubType := uint16(ble.TypeScooterInfo) + uint16(ble.TypeMileage)
	expectedVersionSubType := uint16(ble.TypeScooterInfo) + uint16(ble.TypeSoftwareVersion)

	switch absSubTypeKey {
	case expectedMileageSubType:
		if mileage, ok := convertToInt(value); ok {
			log.Printf("Received mileage: %d", mileage)
			if err := s.redis.WriteInt(KeyMileage, "odometer", mileage); err != nil { // Corrected key/field
				log.Printf("Failed to update mileage in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode mileage value: %v", value)
		}
	case expectedVersionSubType:
		if versionStr, ok := convertToString(value); ok {
			log.Printf("Received software version: %s", versionStr)
			if err := s.redis.WriteString(KeyFirmwareVersion, "mdb-version", versionStr); err != nil {
				log.Printf("Failed to update software version in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode software version value: %v", value)
		}
	default:
		log.Printf("Unknown Scooter Info message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleBLEVersionMessage handles BLE version messages
func (s *Service) handleBLEVersionMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	log.Printf("Handling BLE version message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	expectedAbsSubType := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionString) // 0xA001

	if absSubTypeKey == expectedAbsSubType {
		if versionStr, ok := convertToString(value); ok {
			log.Printf("Received BLE version: %s", versionStr)
			if err := s.redis.WriteString(KeyBLEStatus, "nrf-fw-version", versionStr); err != nil {
				log.Printf("Failed to update BLE version in Redis: %v", err)
			}
		} else {
			log.Printf("Received BLE version with unexpected value type: %T", value)
		}
	} else {
		log.Printf("Unknown BLE version message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleBLEDebugMessage handles BLE debug messages
func (s *Service) handleBLEDebugMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	log.Printf("Handling BLE debug message (Type 0x%04x) with absolute subtype key: 0x%04x, value: %v", msgType, absSubTypeKey, value)

	expectedResetAckSubType := uint16(ble.TypeBLEDebug) + uint16(ble.TypeBLEDebugResetAck) // 0xA023
	expectedResetInfoSubType := uint16(ble.TypeBLEReset) // 0xA021 - Assuming TypeBLEReset is the key for reset info

	switch absSubTypeKey {
	case expectedResetAckSubType:
		log.Printf("Received BLE Debug Reset Ack")
		// Handle ack if needed (e.g., stop a sync timer)
	case expectedResetInfoSubType:
		if resetInfoArr, ok := value.([]interface{}); ok && len(resetInfoArr) == 2 {
			reason, reasonOk := convertToInt(resetInfoArr[0])
			count, countOk := convertToInt(resetInfoArr[1])
			if reasonOk && countOk {
				log.Printf("Received nRF Reset Info: Reason=0x%X, Count=%d", reason, count)
				// Store reason and count in Redis
				if err := s.redis.WriteInt(KeyPowerManager, "nrf-reset-count", count); err != nil {
					log.Printf("Failed to write nrf-reset-count to Redis: %v", err)
				}
				// Publish reason
				if err := s.redis.WriteAndPublishInt(KeyPowerManager, "nrf-reset-reason", reason); err != nil {
					log.Printf("Failed to write/publish nrf-reset-reason to Redis: %v", err)
				}
				// Send ACK back to nRF
				if err := writeUARTMessage(s.usock, ble.TypeBLEDebug, ble.TypeBLEDebugResetAck, 0); err != nil {
					log.Printf("Failed to send Reset ACK to nRF: %v", err)
				} else {
					log.Printf("Sent Reset ACK to nRF")
				}
			} else {
				log.Printf("Could not decode nRF Reset Info array values: %v", resetInfoArr)
			}
		} else {
			log.Printf("Received nRF Reset Info with unexpected value type or length: %T, %v", value, value)
		}

	default:
		log.Printf("Unknown BLE debug message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleDataStreamMessage handles data stream messages
func (s *Service) handleDataStreamMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling data stream message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeDataStreamEnable:
		if enabled, ok := convertToInt(value); ok {
			enabledBool := enabled != 0
			log.Printf("Received data stream enable status update: %t", enabledBool)
			if err := s.redis.WriteInt("aux-battery", "data-stream-enable", enabled); err != nil {
				log.Printf("Failed to write data-stream-enable to Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode data stream enable value: %v", value)
		}

	case ble.TypeDataStreamSync:
		if syncVal, ok := convertToInt(value); ok {
			log.Printf("Received data stream sync confirmation/value: %d", syncVal)
			// Handle sync confirmation if needed
		} else {
			log.Printf("Could not decode data stream sync value: %v", value)
		}

	default:
		log.Printf("Unknown data stream message relative subtype: %v", subType)
	}
}

// handleAuxBatteryMessage handles auxiliary battery messages
func (s *Service) handleAuxBatteryMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling aux battery message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeAuxBatteryVoltage:
		if voltage, ok := convertToInt(value); ok {
			log.Printf("Received aux battery voltage: %d", voltage)
			if err := s.redis.WriteInt("aux-battery", "voltage", voltage); err != nil { // Don't publish voltage often
				log.Printf("Failed to update aux battery voltage in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode aux battery voltage value: %v", value)
		}

	case ble.TypeAuxBatteryCharge:
		if charge, ok := convertToInt(value); ok {
			log.Printf("Received aux battery charge: %d %%", charge)
			if err := s.redis.WriteInt("aux-battery", "charge", charge); err != nil { // Don't publish charge often
				log.Printf("Failed to update aux battery charge in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode aux battery charge value: %v", value)
		}

	case ble.TypeAuxBatteryChargerStatus:
		if statusStr, ok := convertToString(value); ok {
			log.Printf("Received aux battery charger status: %s", statusStr)
			if err := s.redis.WriteString("aux-battery", "charge-status", statusStr); err != nil { // Don't publish status often
				log.Printf("Failed to update aux battery charger status in Redis: %v", err)
			}
		} else {
			log.Printf("Could not decode aux battery charger status value: %v", value)
		}

	default:
		log.Printf("Unknown aux battery message relative subtype: %v", subType)
	}
}

// handlePowerManagementMessage handles power management messages
func (s *Service) handlePowerManagementMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling power management message with relative subtype: %v, value: %v", subType, value)

	switch subType {
	case ble.TypePowerManagementState:
		if stateAck, ok := convertToInt(value); ok {
			log.Printf("Received ACK for Power Management State update: %d", stateAck)
		} else {
			log.Printf("Could not decode power management state ACK value: %v", value)
		}
	case ble.TypePowerManagementPowerRequest:
		if levelAck, ok := convertToInt(value); ok {
			log.Printf("Received ACK for Power Management Power Request update: %d", levelAck)
		} else {
			log.Printf("Could not decode power management power request ACK value: %v", value)
		}
	default:
		log.Printf("Unknown power management message relative subtype: %v", subType)
	}
}

func (s *Service) handleBLEParamMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	log.Printf("Handling BLE param message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	// Define expected absolute subtype values for clarity
	expectedMACSubType := uint16(ble.TypeBLEParam) + uint16(ble.TypeBLEParamMACAddress) // 0xA081
	expectedParamDataSubType := uint16(ble.TypeBLEParam) + uint16(ble.TypeBLEParamData) // 0xA098

	switch absSubTypeKey {
	case expectedMACSubType: // 0xA081
		if macAddrStr, ok := convertToString(value); ok {
			log.Printf("Received BLE MAC address: %s", macAddrStr)
			if err := s.redis.WriteString(KeyBLEStatus, "mac-address", macAddrStr); err != nil {
					log.Printf("Failed to update BLE MAC address in Redis: %v", err)
			}
		} else {
			log.Printf("Received BLE MAC Address with unexpected value type: %T", value)
		}

	case uint16(ble.TypeBLEPairingPinDisplay): // 0xA082
		// This subtype carries the PIN code as a string.
		if strValue, ok := convertToString(value); ok {
			log.Printf("Received BLE Pairing PIN for display: %s", strValue)
			if err := s.redis.WriteAndPublishString(KeyBLEPairingPin, "pin-code", strValue); err != nil {
				log.Printf("Failed to update and publish BLE pairing PIN in Redis: %v", err)
			}
		} else {
			log.Printf("Received BLE Pairing PIN display request with unexpected value type: %T", value)
		}

	case uint16(ble.TypeBLEPairingPinRemove): // 0xA083
		// This subtype is a command acknowledgement/signal, not necessarily tied to BLEParam message type.
		log.Printf("Received request/ack to remove BLE Pairing PIN from display (Subtype 0x%04x)", absSubTypeKey)
		if _, err := s.redis.HDel(KeyBLEPairingPin, "pin-code"); err != nil {
			log.Printf("Failed to delete pairing pin from Redis: %v", err)
		}
		// Publish empty string to signal deletion
		if err := s.redis.WriteAndPublishString(KeyBLEPairingPin, "pin-code", ""); err != nil {
			log.Printf("Failed to publish pairing pin deletion: %v", err)
		}

	case expectedParamDataSubType: // 0xA098
		log.Printf("Received BLE Param Data (Absolute Subtype Key 0x%04x): %v", absSubTypeKey, value)

	case uint16(ble.TypeBLEStatus): // 0xA084 - Check outer type first
		if msgType == ble.TypeBLEParam {
			if statusStr, ok := convertToString(value); ok {
				log.Printf("Received BLE Status update: %s", statusStr)
				if err := s.redis.WriteString(KeyBLEStatus, "connection-status", statusStr); err != nil {
					log.Printf("Failed to write BLE status to Redis: %v", err)
				}
			} else {
				log.Printf("Received BLE Status with unexpected value type: %T", value)
			}
		} else {
			// This case might not be strictly necessary if 0xA084 always comes with TypeBLEParam (0xA080)
			log.Printf("Received BLE Status subtype 0x%04x with unexpected outer message type 0x%04x", absSubTypeKey, msgType)
		}
	default:
		log.Printf("Unhandled BLE parameter absolute subtype key: 0x%04x for message type 0x%04x", absSubTypeKey, msgType)
	}
}

// handleBatteryInfoMessage handles battery information messages (from TypeBatteryInfo, 0x0060)
func (s *Service) handleBatteryInfoMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling CB Battery Info message with relative subtype: %d (0x%x), value: %v", subType, uint16(ble.TypeBatteryInfo)+uint16(subType), value)

	redisKey := KeyCBBattery // Use constant "cb-battery"
	var redisField string
	var redisValue interface{}
	processed := true // Flag to check if we handled the subtype
	valueInt := 0
	valueStr := ""
	isInt := false
	isString := false
	isBool := false // For 'present' specifically

	// First, try to convert to int, handling different incoming types (int64, uint64, etc.)
	intVal, intOk := convertToInt(value)
	if intOk {
		valueInt = intVal
		isInt = true
	} else {
		// If not convertible to int, try string
		strVal, strOk := convertToString(value)
		if strOk {
			valueStr = strVal
			isString = true
		} else {
			// If neither int nor string, log the original type and don't process further for basic types
			log.Printf("CB Battery Info subtype %d value has unexpected type: %T", subType, value)
			processed = false // Mark as not processed for standard handling below
		}
	}

	// Handle specific subtypes requiring bitmask checks separately
	switch subType {
	case ble.TypeBatteryInfoStatus: // 8
		if isInt {
			log.Printf("Received CB Battery Status alert = %d (0x%X)", valueInt, valueInt)
			// Check bits and write alert string or clear
			if valueInt&MAX1730X_STATUS_CURR_MIN_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 0, "Minimum Current Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_CURR_MAX_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 1, "Maximum Current Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_VOLT_MIN_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 2, "Minimum Voltage Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_VOLT_MAX_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 3, "Maximum Voltage Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_TEMP_MIN_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 4, "Minimum Temperature Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_TEMP_MAX_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 5, "Maximum Temperature Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_SOC_MIN_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 6, "Minimum SOC Alert Threshold Exceeded", "alert")
			} else if valueInt&MAX1730X_STATUS_SOC_MAX_ALERT != 0 {
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 7, "Maximum SOC Alert Threshold Exceeded", "alert")
			} else if (valueInt & CB_BATTERY_STATUS_FILTER) == 0 { // If no specific bits are set
				s.writeFaultToRedis(KeyCBBatteryAlert, KeyCBBattery, 0xFF, "", "alert") // Clear alert
			} else {
				log.Printf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_STATUS alert bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			log.Printf("Received CB Battery Status with non-integer value: %v", value)
		}
		return // Handled, exit switch

	case ble.TypeBatteryInfoProtectionStatus: // 11
		if isInt {
			log.Printf("Received CB Battery Prot Status = %d (0x%X)", valueInt, valueInt)
			// Check bits and write fault string or clear
			dischargeFault := (valueInt&MAX1730X_PROTSTATUS_ODCP != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_UVP != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_TOOHOTD != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_DIEHOT != 0)

			chargeFault := (valueInt&MAX1730X_PROTSTATUS_TOOCOLDC != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_OVP != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_OCCP != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_QOVFLW != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_TOOHOTC != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_FULL != 0) ||
				(valueInt&MAX1730X_PROTSTATUS_DIEHOT != 0)

			if dischargeFault {
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 0, "Discharging fault", "fault")
			} else if chargeFault {
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 1, "Charging fault", "fault")
			} else if (valueInt & CB_BATTERY_PROTECTION_STATUS_FILTER) == 0 { // If no specific bits are set
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 0xFF, "", "fault") // Clear fault
			} else {
				log.Printf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_PROT_STATUS bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			log.Printf("Received CB Battery Prot Status with non-integer value: %v", value)
		}
		return // Handled, exit switch

	case ble.TypeBatteryInfoBattStatus: // 15
		if isInt {
			log.Printf("Received CB Battery Batt Status = %d (0x%X)", valueInt, valueInt)
			// Check bits and write fault string or clear
			if valueInt&MAX1730X_BATTSTATUS_CHG_FET_FAIL != 0 {
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 2, "ChargeFET Failure-Short Detected", "fault")
			} else if valueInt&MAX1730X_BATTSTATUS_DISCHG_FET_FAIL != 0 {
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 3, "DischargeFET Failure-Short Detected", "fault")
			} else if valueInt&MAX1730X_BATTSTATUS_FET_FAIL_OPEN != 0 {
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 4, "FET Failure open", "fault")
			} else if (valueInt & CB_BATTERY_BATT_STATUS_FILTER) == 0 { // If no specific bits are set
				s.writeFaultToRedis(KeyCBBatteryFault, KeyCBBattery, 0xFF, "", "fault") // Clear fault
			} else {
				log.Printf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_BATT_STATUS bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			log.Printf("Received CB Battery Batt Status with non-integer value: %v", value)
		}
		return // Handled, exit switch
	}

	// --- Standard handling for other subtypes ---
	if !processed {
		// If initial type conversion failed and it wasn't handled above, just return.
		return
	}

	// Set redisField and redisValue based on subtype for standard types
	processedStandard := true // Separate flag for standard processing
	switch subType {
	case ble.TypeBatteryInfoCharge: // 1
		redisField = "charge"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoCurrent: // 2
		redisField = "current"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoRemCapacity: // 3
		redisField = "remaining-capacity"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoFullCapacity: // 4
		redisField = "full-capacity"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoCellVoltage: // 5
		redisField = "cell-voltage"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoTemp: // 6
		redisField = "temperature"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoCycleCount: // 7
		redisField = "cycle-count"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoTTE: // 9
		redisField = "time-to-empty"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoTTF: // 10
		redisField = "time-to-full"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoSOH: // 12
		redisField = "state-of-health"
		if isInt { redisValue = valueInt } else { processedStandard = false }
	case ble.TypeBatteryInfoUniqueID: // 13
		redisField = "unique-id"
		if isString { redisValue = valueStr } else { processedStandard = false }
	case ble.TypeBatteryInfoSerialNumber: // 14
		redisField = "serial-number"
		if isString { redisValue = valueStr } else { processedStandard = false }
	case ble.TypeBatteryInfoPartNo: // 16
		redisField = "part-number"
		if isInt {
			switch valueInt {
			case 5: valueStr = "MAX17301"
			case 6: valueStr = "MAX17302"
			case 7: valueStr = "MAX17303"
			default: valueStr = fmt.Sprintf("MAX1730X (%d)", valueInt) // Include raw value if unknown
			}
			redisValue = valueStr
			isString = true // Mark as string for writing
			isInt = false
			log.Printf("Received CB Battery Part Number: %s (from int %d)", valueStr, valueInt)
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoPresent: // 17
		redisField = "present"
		if isInt {
			presentBool := valueInt != 0
			log.Printf("Received CB Battery Present: %t", presentBool)
			redisValue = presentBool // Store bool type
			isBool = true // Mark as bool for Redis write switch
			isInt = false // Unmark as int
		} else { processedStandard = false }
	case ble.TypeBatteryInfoChargeStatus: // 18
		redisField = "charge-status"
		if isInt {
			switch valueInt {
			case 0: valueStr = "not-charging"
			case 1: valueStr = "charging"
			default: valueStr = "unknown"
			}
			redisValue = valueStr
			isString = true // Mark as string for writing
			isInt = false
			log.Printf("Received CB Battery Charge Status: %s (from int %d)", valueStr, valueInt)
		} else { processedStandard = false }
	default:
		log.Printf("Unknown or already handled CB Battery Info relative subtype: %d", subType)
		processedStandard = false
	}

	// Write standard types to Redis only if processed and field name is set
	if processedStandard && redisField != "" {
		var err error
		if isInt {
			err = s.redis.WriteInt(redisKey, redisField, valueInt)
		} else if isString {
			err = s.redis.WriteString(redisKey, redisField, valueStr)
		} else if isBool {
			// Convert bool to "true"/"false" string for Redis consistency
			strVal := "false"
			if redisValue.(bool) {
				strVal = "true"
			}
			err = s.redis.WriteString(redisKey, redisField, strVal)
		} // No else needed, already checked processedStandard

		if err != nil {
			log.Printf("Failed to write %s/%s to Redis: %v", redisKey, redisField, err)
		}
	} else if !processedStandard && redisField != "" { // Log if processing failed for a known standard type
		log.Printf("Could not process value for standard CB Battery subtype %d (Field: %s), Value Type: %T", subType, redisField, value)
	}
}

// handleEventMessage handles generic event messages (Type 0x0000) from the nRF.
// These messages contain strings like "topic:payload" (e.g., "scooter:seatbox open").
func (s *Service) handleEventMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	eventStr, ok := value.(string)
	if !ok {
		log.Printf("Handling Event message: value is not a string: %T", value)
		return
	}
	log.Printf("Received event string: %s", eventStr)

	var listKey string
	var listValue string

	switch eventStr {
	case "scooter:state unlock":
		listKey = "scooter:state"
		listValue = "unlock"
	case "scooter:state lock":
		listKey = "scooter:state"
		listValue = "lock"
	case "scooter:seatbox open":
		listKey = "scooter:seatbox"
		listValue = "open"
	case "scooter:blinker right":
		listKey = "scooter:blinker"
		listValue = "right"
	case "scooter:blinker left":
		listKey = "scooter:blinker"
		listValue = "left"
	case "scooter:blinker both":
		listKey = "scooter:blinker"
		listValue = "both"
	case "scooter:blinker off":
		listKey = "scooter:blinker"
		listValue = "off"
	default:
		log.Printf("Warning: Received unknown event string: %s", eventStr)
		return // Do not push unknown events
	}

	// Perform LPUSH
	err := s.redis.LPush(listKey, listValue)
	if err != nil {
		log.Printf("Failed to LPUSH event '%s' to Redis list '%s': %v", listValue, listKey, err)
	} else {
		log.Printf("LPUSHed event '%s' to Redis list '%s'", listValue, listKey)
	}
}

// handlePowerMuxMessage handles incoming power mux messages
func (s *Service) handlePowerMuxMessage(subType ble.SubType, value interface{}) {
	log.Printf("Handling Power Mux message (Value Type: %T): %v", value, value)

	powerMuxState, ok := convertToInt(value)
	if !ok {
		log.Printf("Received Power Mux message with non-integer value: %v", value)
		return
	}

	var selectedInput string
	if powerMuxState == 0 {
		selectedInput = "aux"
	} else {
		selectedInput = "cb"
	}
	log.Printf("Received PowerMux update: selected-input=%s (raw value: %d)", selectedInput, powerMuxState)

	// Publish the string value to Redis
	err := s.redis.WriteAndPublishString("power-mux", "selected-input", selectedInput)
	if err != nil {
		log.Printf("Error publishing PowerMux state to Redis: %v", err)
	}
}

func (s *Service) writeFaultToRedis(key, source string, code int, message, faultType string) {
	field := faultType // Use "alert" or "fault" as the field name directly

	if code == 0xFF && message == "" { // Clear fault/alert
		log.Printf("Clearing Redis field: %s for key %s", field, key)
		_, err := s.redis.HDel(key, field)
		if err != nil && err != redis.Nil {
			log.Printf("Error clearing Redis field %s for key %s: %v", field, key, err)
		}
	} else {
		log.Printf("Writing Redis field: %s = '%s' for key %s (Code: %d, Source: %s)", field, message, key, code, source)
		err := s.redis.WriteString(key, field, message) // Just write the message string
		if err != nil {
			log.Printf("Error writing Redis field %s for key %s: %v", field, key, err)
		}
	}
}

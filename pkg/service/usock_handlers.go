package service

import (
	"fmt"
	"strconv"
	"strings"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/usock"
)

// HandleUSockMessage handles incoming USOCK messages
func (s *Service) HandleUSockMessage(frameID byte, payload *usock.Payload) {
	var msgData map[uint16]interface{}
	err := cbor.Unmarshal(payload.Data, &msgData)
	if err != nil {
		s.log.Errorf("Failed to decode CBOR message: %v (raw: %x)", err, payload.Data)
		return
	}

	s.log.Debugf("Received message: Frame ID=0x%02x, Data=%x", frameID, payload.Data)

	var absoluteMsgType uint16
	var params interface{}
	// Find the single top-level key which represents the absolute message type (e.g., 0xA000 for version)
	if len(msgData) != 1 {
		if len(msgData) == 0 && len(payload.Data) == 4 && payload.Data[0] == 0xa1 && payload.Data[2] == frameID && payload.Data[3] == 0xa0 {
			s.log.Debugf("Received acknowledgment (empty map) for Frame ID: 0x%02x", frameID)
			switch frameID {
			case byte(ble.TypeDataStream & 0xFF): // 0xC0
				s.log.Debugf("Received acknowledgment for Data Stream command")
			case byte(ble.TypeBattery & 0xFF): // 0xE0
				s.log.Debugf("Received acknowledgment for Battery command")
			case byte(ble.TypeBLECommand & 0xFF): // 0xAA
				s.log.Debugf("Received acknowledgment for BLE Command")
			case byte(ble.TypeVehicleState & 0xFF): // 0x20 - Also overlaps with TypeBLEDebug
				s.log.Debugf("Received acknowledgment for command with Frame ID 0x20 (Could be Vehicle State or BLE Debug)")
			// Note: Frame ID 0x00 overlaps TypePowerManagement and TypeBLEVersion
			case 0x40:
				s.log.Debugf("Received acknowledgment for command with Frame ID 0x40 (Could be Scooter Info or Aux Battery)")
			case byte(ble.TypeBLEParam & 0xFF): // 0x80
				s.log.Debugf("Received acknowledgment for BLE Param command")
			// Removed specific cases for overlapping Frame IDs (0x00, 0x20, 0x40) that evaluated to the same byte.
			default:
				s.log.Debugf("Received unknown acknowledgment type via Frame ID 0x%02x", frameID)
			}
			return // Processing finished for simple ACK
		} else {
			s.log.Infof("Received message with unexpected top-level structure (expected 1 key): %d keys", len(msgData))
			return
		}
	}

	for k, v := range msgData {
		absoluteMsgType = k // This is the absolute message type (e.g., 0xA000, 0xA080)
		params = v
		break
	}

	msgType := ble.MessageType(absoluteMsgType)

	// Decode the inner parameter map (key should be absolute subtype)
	interMap, okInter := params.(map[interface{}]interface{})
	if !okInter {
		s.log.Infof("Received message with non-map parameter type: %T", params)
		return
	}

	// Convert inner map keys (likely uint64) to absolute uint16 subtype keys
	paramMap := make(map[uint16]interface{})
	for k, v := range interMap {
		keyInt, okKey := k.(uint64)
		if !okKey {
			s.log.Infof("Received message with non-uint64 key in parameter map: %T, Key: %v", k, k)
			continue
		}
		if keyInt > 0xFFFF {
			s.log.Warnf(" Received subtype key %d (0x%x) larger than uint16, skipping.", keyInt, keyInt)
			continue
		}
		paramMap[uint16(keyInt)] = v // Key is the ABSOLUTE subtype (e.g., 0xA001)
	}

	// Log decoded CBOR structure with MessageType and SubTypes
	if len(paramMap) > 0 {
		subtypeStr := ""
		for absSubType, value := range paramMap {
			if subtypeStr != "" {
				subtypeStr += ", "
			}
			// Format value based on type
			switch v := value.(type) {
			case int64:
				subtypeStr += fmt.Sprintf("0x%04x=%d", absSubType, v)
			case uint64:
				subtypeStr += fmt.Sprintf("0x%04x=%d", absSubType, v)
			case string:
				subtypeStr += fmt.Sprintf("0x%04x=\"%s\"", absSubType, v)
			case []interface{}:
				subtypeStr += fmt.Sprintf("0x%04x=[%d items]", absSubType, len(v))
			default:
				subtypeStr += fmt.Sprintf("0x%04x=<%T>", absSubType, v)
			}
		}
		s.log.Debugf("Received: Type=0x%04x %s", msgType, subtypeStr)
	} else {
		s.log.Debugf("Received: Type=0x%04x (empty params)", msgType)
	}

	// Handle messages based on the ABSOLUTE subtype key found in the inner map
	if len(paramMap) > 0 {
		for absSubTypeKey, value := range paramMap {
			s.log.Debugf("Decoded Absolute Subtype Key: 0x%04x, Value Type: %T", absSubTypeKey, value)

			// Calculate the expected relative subtype for internal logic if needed,
			// but routing should primarily use the absolute key or outer msgType.
			relativeSubType := ble.SubType(0) // Initialize
			// Avoid potential underflow if absSubTypeKey is somehow less than msgType
			if absSubTypeKey >= uint16(msgType) {
				relativeSubType = ble.SubType(absSubTypeKey - uint16(msgType)) // Calculate relative for case matching
			} else {
				s.log.Warnf(" Absolute subtype key 0x%04x is less than message type 0x%04x", absSubTypeKey, msgType)
			}

			switch msgType { // Route based on outer message type
			case ble.TypeBattery:
				// Battery handler needs to determine slot from absolute subtype key
				slot := -1
				// Determine slot based on ABSOLUTE key range from ble/types.go definitions
				if absSubTypeKey >= (uint16(ble.TypeBattery)+uint16(ble.TypeBatterySlot0State)) &&
					absSubTypeKey <= (uint16(ble.TypeBattery)+uint16(ble.TypeBatterySlot0Charge)) { // Simplified range check
					slot = 0
				} else if absSubTypeKey >= (uint16(ble.TypeBattery)+uint16(ble.TypeBatterySlot1State)) &&
					absSubTypeKey <= (uint16(ble.TypeBattery)+uint16(ble.TypeBatterySlot1Charge)) { // Simplified range check
					slot = 1
				}
				if slot >= 0 {
					// Pass the RELATIVE subtype to the handler for case matching
					s.handleBatteryMessage(relativeSubType, value, slot)
				} else {
					s.log.Warnf("Could not determine battery slot for absolute subtype key 0x%04x", absSubTypeKey)
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
			case ble.TypePowerManagementHibernationReq: // Direct hibernation request (0x0803)
				s.handlePowerManagementMessage(ble.TypePowerManagementHibernationRequest, value)
			case ble.TypeAccelerometer:
				s.handleAccelerometerMessage(relativeSubType, value)
			case ble.TypeExtended:
				s.handleExtendedCommandMessage(msgType, absSubTypeKey, value)
			case 0x0000: // Handle generic event messages
				s.handleEventMessage(msgType, absSubTypeKey, value)
			default:
				s.log.Warnf("Unhandled message type: 0x%04x with absolute subtype key 0x%04x", msgType, absSubTypeKey)
			}
		}
	} else {
		s.log.Infof("Received message type 0x%04x with empty parameter map (might be ACK)", msgType)
		// Handle parameterless messages if needed (e.g., some specific ACKs not caught earlier)
	}
}

// handleBLECommandMessage handles incoming BLE command acknowledgments/responses
func (s *Service) handleBLECommandMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	s.log.Debugf("Handling BLE command message (Type 0x%04x) with absolute subtype key: 0x%04x, value: %v", msgType, absSubTypeKey, value)

	// Determine relative command based on absolute key
	var relativeCmd ble.BLECommand
	if absSubTypeKey >= uint16(ble.TypeBLECommand) && absSubTypeKey < (uint16(ble.TypeBLECommand)+10) { // Rough check
		relativeCmd = ble.BLECommand(absSubTypeKey - uint16(ble.TypeBLECommand))
	} else {
		s.log.Warnf(" Could not determine relative BLE command for absolute key 0x%04x", absSubTypeKey)
		return
	}

	switch relativeCmd {
	case ble.BLECommandAdvStartWithWhitelist:
		s.log.Debugf("Received acknowledgment for Start Advertising (Whitelist) command.")
	case ble.BLECommandAdvRestartNoWhitelist:
		s.log.Debugf("Received acknowledgment for Restart Advertising (No Whitelist) command.")
	case ble.BLECommandAdvStop:
		s.log.Debugf("Received acknowledgment for Stop Advertising command.")
	case ble.BLECommandDeleteBond:
		s.log.Debugf("Received acknowledgment for Delete Bond command.")
	case ble.BLECommandDeleteAllBonds:
		s.log.Debugf("Received acknowledgment for Delete All Bonds command.")
	default:
		s.log.Warnf("Unknown relative BLE command subtype: %d (Absolute Key: 0x%04x)", relativeCmd, absSubTypeKey)
	}
}

// handleBatteryMessage handles battery-related messages
func (s *Service) handleBatteryMessage(subType ble.SubType, value interface{}, slot int) {
	s.log.Debugf("Handling battery message with subtype: %v for slot: %d", subType, slot)
	/*redisKey := KeyBatterySlot1
	if slot == 2 {
		redisKey = KeyBatterySlot2
	}*/

	switch subType {
	case ble.TypeBatterySlot0State, ble.TypeBatterySlot1State:
		if state, ok := convertToInt(value); ok {
			s.log.Debugf("Received battery state for slot %d: %d (%s)", slot, state, batteryStateToString(state))
			// Note: battery-service is authoritative for state, we don't write it
		} else {
			s.log.Warnf("Could not decode battery state value: %v", value)
		}

	case ble.TypeBatterySlot0Presence, ble.TypeBatterySlot1Presence:
		if present, ok := convertToInt(value); ok {
			presentBool := present != 0
			s.log.Debugf("Received battery presence for slot %d: %t", slot, presentBool)
			// Note: battery-service is authoritative for presence, we don't write it
		} else {
			s.log.Warnf("Could not decode battery presence value: %v", value)
		}

	case ble.TypeBatterySlot0CycleCount, ble.TypeBatterySlot1CycleCount:
		if count, ok := convertToInt(value); ok {
			s.log.Debugf("Received battery cycle count for slot %d: %d", slot, count)
			// Note: battery-service is authoritative for cycle-count, we don't write it
		} else {
			s.log.Warnf("Could not decode battery cycle count value: %v", value)
		}

	case ble.TypeBatterySlot0Charge, ble.TypeBatterySlot1Charge:
		if charge, ok := convertToInt(value); ok {
			s.log.Debugf("Received battery charge for slot %d: %d %%", slot, charge)
			// Note: battery-service is authoritative for charge, we don't write it
		} else {
			s.log.Warnf("Could not decode battery charge value: %v", value)
		}

	default:
		s.log.Warnf("Unknown battery message relative subtype: %v (Absolute Key was processed)", subType)
	}
}

// handleVehicleStateMessage handles vehicle state messages
func (s *Service) handleVehicleStateMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling vehicle state message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeVehicleStateState:
		if state, ok := convertToInt(value); ok {
			s.log.Debugf("Received vehicle state: %d", state)
		} else {
			s.log.Warnf("Could not decode vehicle state value: %v", value)
		}

	case ble.TypeVehicleStateSeatbox:
		if state, ok := convertToInt(value); ok {
			s.log.Debugf("Received seatbox state: %d", state)
		} else {
			s.log.Warnf("Could not decode seatbox state value: %v", value)
		}

	case ble.TypeVehicleStateHandlebar:
		if state, ok := convertToInt(value); ok {
			s.log.Debugf("Received handlebar lock state: %d", state)
		} else {
			s.log.Warnf("Could not decode handlebar lock state value: %v", value)
		}

	default:
		s.log.Warnf("Unknown vehicle state message relative subtype: %v", subType)
	}
}

// handleScooterInfoMessage handles scooter information messages (Mileage, SW Version)
func (s *Service) handleScooterInfoMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	s.log.Debugf("Handling Scooter Info message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	expectedMileageSubType := uint16(ble.TypeScooterInfo) + uint16(ble.TypeMileage)
	expectedVersionSubType := uint16(ble.TypeScooterInfo) + uint16(ble.TypeSoftwareVersion)

	switch absSubTypeKey {
	case expectedMileageSubType:
		// Try string first, then fallback to int if needed
		if mileageStr, ok := convertToString(value); ok {
			// Convert string to int
			if mileage, err := strconv.Atoi(mileageStr); err == nil {
				s.log.Debugf("Received mileage (string): %d", mileage)
			if err := s.ipc.Hash(KeyMileage).Set("odometer", fmt.Sprintf("%d", mileage)); err != nil {
					s.log.Errorf("Failed to update mileage in Redis: %v", err)
				}
			} else {
				s.log.Warnf("Could not convert mileage string to int: %v", err)
			}
		} else if mileage, ok := convertToInt(value); ok {
			// Fallback to direct int handling
			s.log.Debugf("Received mileage (int): %d", mileage)
			if err := s.ipc.Hash(KeyMileage).Set("odometer", fmt.Sprintf("%d", mileage)); err != nil {
				s.log.Errorf("Failed to update mileage in Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode mileage value: %v", value)
		}
	case expectedVersionSubType:
		if versionStr, ok := convertToString(value); ok {
			s.log.Debugf("Received software version: %s", versionStr)
				if err := s.ipc.Hash(KeyFirmwareVersion).Set("mdb-version", versionStr); err != nil {
				s.log.Errorf("Failed to update software version in Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode software version value: %v", value)
		}
	default:
		s.log.Warnf("Unknown Scooter Info message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleBLEVersionMessage handles BLE version messages
func (s *Service) handleBLEVersionMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	s.log.Debugf("Handling BLE version message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	expectedAbsSubType := uint16(ble.TypeBLEVersion) + uint16(ble.TypeBLEVersionString) // 0xA001

	if absSubTypeKey == expectedAbsSubType {
		if versionStr, ok := convertToString(value); ok {
			s.log.Infof("Detected firmware version: %s", versionStr)

			// Write version to Redis immediately (before compatibility check)
				if err := s.ipc.Hash(KeyFirmwareVersion).Set("nrf-fw-version", versionStr); err != nil {
				s.log.Errorf("Failed to update BLE version in Redis: %v", err)
			}

			// Parse version
			fwVersion, err := ParseFirmwareVersion(versionStr)
			if err != nil {
				s.log.Errorf("Failed to parse firmware version: %v", err)
				s.setErrorAndSleep("Invalid firmware version format")
				return
			}

			// Check compatibility
			if !fwVersion.IsCompatible() {
				s.log.Errorf("Firmware version %s incompatible (minimum: v%d.%d.%d)",
					versionStr, MinMajor, MinMinor, MinPatch)
				s.setErrorAndSleep(fmt.Sprintf(
					"Firmware version %s not supported. Minimum required: v%d.%d.%d. Please upgrade firmware.",
					versionStr, MinMajor, MinMinor, MinPatch))
				return
			}

			s.log.Infof("Firmware version %s is compatible", versionStr)

			// Trigger firmware update check if auto-update is enabled
			if s.GetAutoUpdate() {
				fu := s.GetFirmwareUpdater()
				if fu != nil {
					go func() {
						updated, err := fu.CheckAndUpdate(versionStr)
						if err != nil {
							s.log.Errorf("Firmware update check failed: %v", err)
						} else if updated {
							s.log.Infof("Firmware was updated successfully")
						}
					}()
				}
			}
		} else {
			s.log.Infof("Received BLE version with unexpected value type: %T", value)
		}
	} else {
		s.log.Warnf("Unknown BLE version message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleBLEDebugMessage handles BLE debug messages
func (s *Service) handleBLEDebugMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	s.log.Debugf("Handling BLE debug message (Type 0x%04x) with absolute subtype key: 0x%04x, value: %v", msgType, absSubTypeKey, value)

	expectedResetAckSubType := uint16(ble.TypeBLEDebug) + uint16(ble.TypeBLEDebugResetAck) // 0xA023
	expectedResetInfoSubType := uint16(ble.TypeBLEReset)                                   // 0xA021 - Assuming TypeBLEReset is the key for reset info

	switch absSubTypeKey {
	case expectedResetAckSubType:
		s.log.Debugf("Received BLE Debug Reset Ack")
		// Handle ack if needed (e.g., stop a sync timer)
	case expectedResetInfoSubType:
		if resetInfoArr, ok := value.([]interface{}); ok && len(resetInfoArr) == 2 {
			reason, reasonOk := convertToInt(resetInfoArr[0])
			count, countOk := convertToInt(resetInfoArr[1])
			if reasonOk && countOk {
				s.log.Debugf("Received nRF Reset Info: Reason=0x%X, Count=%d", reason, count)
				// Store reason and count in Redis
					if err := s.ipc.Hash(KeyPowerManager).Set("nrf-reset-count", fmt.Sprintf("%d", count)); err != nil {
					s.log.Errorf("Failed to write nrf-reset-count to Redis: %v", err)
				}
				// Publish reason
				
			if err := s.ipc.Hash(KeyPowerManager).Set("nrf-reset-reason", fmt.Sprintf("%d", reason)); err != nil {
					s.log.Errorf("Failed to write/publish nrf-reset-reason to Redis: %v", err)
				}
				// Send ACK back to nRF
				if err := writeUARTMessage(s.usock, ble.TypeBLEDebug, ble.TypeBLEDebugResetAck, 0); err != nil {
					s.log.Errorf("Failed to send Reset ACK to nRF: %v", err)
				} else {
					s.log.Debugf("Sent Reset ACK to nRF")
				}
			} else {
				s.log.Warnf("Could not decode nRF Reset Info array values: %v", resetInfoArr)
			}
		} else {
			s.log.Infof("Received nRF Reset Info with unexpected value type or length: %T, %v", value, value)
		}

	default:
		s.log.Warnf("Unknown BLE debug message absolute subtype key: 0x%04x", absSubTypeKey)
	}
}

// handleDataStreamMessage handles data stream messages
func (s *Service) handleDataStreamMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling data stream message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeDataStreamEnable:
		if enabled, ok := convertToInt(value); ok {
			enabledBool := enabled != 0
			s.log.Debugf("Received data stream enable status update: %t", enabledBool)
				if err := s.ipc.Hash("aux-battery").Set("data-stream-enable", fmt.Sprintf("%d", enabled)); err != nil {
				s.log.Errorf("Failed to write data-stream-enable to Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode data stream enable value: %v", value)
		}

	case ble.TypeDataStreamSync:
		if syncVal, ok := convertToInt(value); ok {
			s.log.Debugf("Received data stream sync confirmation/value: %d", syncVal)
			// Handle sync confirmation if needed
		} else {
			s.log.Warnf("Could not decode data stream sync value: %v", value)
		}

	default:
		s.log.Warnf("Unknown data stream message relative subtype: %v", subType)
	}
}

// handleAuxBatteryMessage handles auxiliary battery messages
func (s *Service) handleAuxBatteryMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling aux battery message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeAuxBatteryVoltage:
		if voltage, ok := convertToInt(value); ok {
			s.log.Debugf("Received aux battery voltage: %d", voltage)
				if err := s.ipc.Hash("aux-battery").Set("voltage", fmt.Sprintf("%d", voltage)); err != nil { // Don't publish voltage often
				s.log.Errorf("Failed to update aux battery voltage in Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode aux battery voltage value: %v", value)
		}

	case ble.TypeAuxBatteryCharge:
		if charge, ok := convertToInt(value); ok {
			s.log.Debugf("Received aux battery charge: %d %%", charge)
				if err := s.ipc.Hash("aux-battery").Set("charge", fmt.Sprintf("%d", charge)); err != nil { // Don't publish charge often
				s.log.Errorf("Failed to update aux battery charge in Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode aux battery charge value: %v", value)
		}

	case ble.TypeAuxBatteryChargerStatus:
		if statusStr, ok := convertToString(value); ok {
			s.log.Debugf("Received aux battery charger status: %s", statusStr)
				if err := s.ipc.Hash("aux-battery").Set("charge-status", statusStr); err != nil { // Don't publish status often
				s.log.Errorf("Failed to update aux battery charger status in Redis: %v", err)
			}
		} else {
			s.log.Warnf("Could not decode aux battery charger status value: %v", value)
		}

	default:
		s.log.Warnf("Unknown aux battery message relative subtype: %v", subType)
	}
}

// handlePowerManagementMessage handles power management messages
func (s *Service) handlePowerManagementMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling power management message with relative subtype: %v, value: %v", subType, value)

	switch subType {
	case ble.TypePowerManagementState:
		if stateAck, ok := convertToInt(value); ok {
			s.log.Debugf("Received ACK for Power Management State update: %d", stateAck)
		} else {
			s.log.Warnf("Could not decode power management state ACK value: %v", value)
		}
	case ble.TypePowerManagementPowerRequest:
		if levelAck, ok := convertToInt(value); ok {
			s.log.Debugf("Received ACK for Power Management Power Request update: %d", levelAck)
		} else {
			s.log.Warnf("Could not decode power management power request ACK value: %v", value)
		}
	case ble.TypePowerManagementHibernationRequest:
		if hibernationType, ok := convertToInt(value); ok {
			hibernationTypeStr := "automatic"
			if hibernationType == int(ble.HibernationRequestManual) {
				hibernationTypeStr = "manual"
			}
			s.log.Debugf("Received hibernation request from nRF: type=%s (%d)", hibernationTypeStr, hibernationType)

			// Forward hibernation request to power manager via Redis
			var command string
			var listKey string

			if hibernationType == int(ble.HibernationRequestManual) {
				// Check vehicle state for manual hibernation
				vehicleState, err := s.ipc.HGet(KeyVehicle, "state")
				if err == nil && vehicleState == "parked" {
					// If parked, send lock-hibernate to vehicle state handler
					listKey = "scooter:state"
					command = "lock-hibernate"
				} else {

					// Otherwise send to power manager
					listKey = "scooter:power"
					command = "hibernate-manual"
				}
			} else {
				// Automatic hibernation always goes to power manager
				listKey = "scooter:power"
				command = "hibernate"
			}

			if err := ipc.SendRequest(s.ipc, listKey, command); err != nil {
				s.log.Errorf("Failed to forward hibernation request to %s: %v", listKey, err)
			} else {
				s.log.Debugf("Forwarded hibernation request to %s: %s", listKey, command)
			}
		} else {
			s.log.Warnf("Could not decode hibernation request value: %v", value)
		}
	default:
		s.log.Warnf("Unknown power management message relative subtype: %v", subType)
	}
}

func (s *Service) handleBLEParamMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	s.log.Debugf("Handling BLE param message (Type 0x%04x). Absolute subtype key: 0x%04x", msgType, absSubTypeKey)

	// Define expected absolute subtype values for clarity
	expectedMACSubType := uint16(ble.TypeBLEParam) + uint16(ble.TypeBLEParamMACAddress) // 0xA081
	expectedParamDataSubType := uint16(ble.TypeBLEParam) + uint16(ble.TypeBLEParamData) // 0xA098

	switch absSubTypeKey {
	case expectedMACSubType: // 0xA081
		if macAddrStr, ok := convertToString(value); ok {
			s.log.Debugf("Received BLE MAC address: %s", macAddrStr)
				if err := s.ipc.Hash(KeyBLEStatus).Set("mac-address", macAddrStr); err != nil {
				s.log.Errorf("Failed to update BLE MAC address in Redis: %v", err)
			}
		} else {
			s.log.Infof("Received BLE MAC Address with unexpected value type: %T", value)
		}

	case uint16(ble.TypeBLEPairingPinDisplay): // 0xA082
		// This subtype carries the PIN code as a string.
		if strValue, ok := convertToString(value); ok {
			s.log.Debugf("Received BLE Pairing PIN for display: %s", strValue)
				if err := s.ipc.Hash(KeyBLEPairingPin).Set("pin-code", strValue); err != nil {
				s.log.Errorf("Failed to update and publish BLE pairing PIN in Redis: %v", err)
			}
		} else {
			s.log.Infof("Received BLE Pairing PIN display request with unexpected value type: %T", value)
		}

	case uint16(ble.TypeBLEPairingPinRemove): // 0xA083
		// This subtype is a command acknowledgement/signal, not necessarily tied to BLEParam message type.
		s.log.Debugf("Received request/ack to remove BLE Pairing PIN from display (Subtype 0x%04x)", absSubTypeKey)
		if err := s.ipc.Hash(KeyBLEPairingPin).Delete("pin-code"); err != nil {
			s.log.Errorf("Failed to delete pairing pin from Redis: %v", err)
		}
		// Publish empty string to signal deletion
		if err := s.ipc.Hash(KeyBLEPairingPin).Set("pin-code", ""); err != nil {
			s.log.Errorf("Failed to publish pairing pin deletion: %v", err)
		}

	case expectedParamDataSubType: // 0xA098
		s.log.Debugf("Received BLE Param Data (Absolute Subtype Key 0x%04x): %v", absSubTypeKey, value)

	case uint16(ble.TypeBLEStatus): // 0xA084 - Check outer type first
		if msgType == ble.TypeBLEParam {
			if statusStr, ok := convertToString(value); ok {
				s.log.Debugf("Received BLE Status update: %s", statusStr)
					if err := s.ipc.Hash(KeyBLEStatus).Set("connection-status", statusStr); err != nil {
					s.log.Errorf("Failed to write BLE status to Redis: %v", err)
				}
			} else {
				s.log.Infof("Received BLE Status with unexpected value type: %T", value)
			}
		} else {
			// This case might not be strictly necessary if 0xA084 always comes with TypeBLEParam (0xA080)
			s.log.Infof("Received BLE Status subtype 0x%04x with unexpected outer message type 0x%04x", absSubTypeKey, msgType)
		}
	default:
		s.log.Warnf("Unhandled BLE parameter absolute subtype key: 0x%04x for message type 0x%04x", absSubTypeKey, msgType)
	}
}

// handleBatteryInfoMessage handles battery information messages (from TypeBatteryInfo, 0x0060)
func (s *Service) handleBatteryInfoMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling CB Battery Info message with relative subtype: %d (0x%x), value: %v", subType, uint16(ble.TypeBatteryInfo)+uint16(subType), value)

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
			s.log.Warnf("CB Battery Info subtype %d value has unexpected type: %T", subType, value)
			processed = false // Mark as not processed for standard handling below
		}
	}

	// Handle specific subtypes requiring bitmask checks separately
	switch subType {
	case ble.TypeBatteryInfoStatus: // 8
		if isInt {
			s.log.Debugf("Received CB Battery Status alert = %d (0x%X)", valueInt, valueInt)
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
				s.log.Warnf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_STATUS alert bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			s.log.Debugf("Received CB Battery Status with non-integer value: %v", value)
		}
		return // Handled, exit switch

	case ble.TypeBatteryInfoProtectionStatus: // 11
		if isInt {
			s.log.Debugf("Received CB Battery Prot Status = %d (0x%X)", valueInt, valueInt)
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
				s.log.Warnf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_PROT_STATUS bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			s.log.Debugf("Received CB Battery Prot Status with non-integer value: %v", value)
		}
		return // Handled, exit switch

	case ble.TypeBatteryInfoBattStatus: // 15
		if isInt {
			s.log.Debugf("Received CB Battery Batt Status = %d (0x%X)", valueInt, valueInt)
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
				s.log.Warnf("Unhandled BLE_SCOOTER_SERVICE_CB_BATTERY_BATT_STATUS bits: %d (0x%X)", valueInt, valueInt)
			}
		} else {
			s.log.Debugf("Received CB Battery Batt Status with non-integer value: %v", value)
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
		if isInt {
			s.log.Debugf("Received CB Battery Charge: %d %%", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoCurrent: // 2
		redisField = "current"
		if isInt {
			s.log.Debugf("Received CB Battery Current: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoRemCapacity: // 3
		redisField = "remaining-capacity"
		if isInt {
			s.log.Debugf("Received CB Battery Remaining Capacity: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoFullCapacity: // 4
		redisField = "full-capacity"
		if isInt {
			s.log.Debugf("Received CB Battery Full Capacity: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoCellVoltage: // 5
		redisField = "cell-voltage"
		if isInt {
			s.log.Debugf("Received CB Battery Cell Voltage: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoTemp: // 6
		redisField = "temperature"
		if isInt {
			s.log.Debugf("Received CB Battery Temperature: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoCycleCount: // 7
		redisField = "cycle-count"
		if isInt {
			s.log.Debugf("Received CB Battery Cycle Count: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoTTE: // 9
		redisField = "time-to-empty"
		if isInt {
			s.log.Debugf("Received CB Battery Time-To-Empty: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoTTF: // 10
		redisField = "time-to-full"
		if isInt {
			s.log.Debugf("Received CB Battery Time-To-Full: %d", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoSOH: // 12
		redisField = "state-of-health"
		if isInt {
			s.log.Debugf("Received CB Battery State-Of-Health: %d %%", valueInt)
			redisValue = valueInt
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoUniqueID: // 13
		redisField = "unique-id"
		if isString {
			s.log.Debugf("Received CB Battery Unique ID: %s", valueStr)
			redisValue = valueStr
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoSerialNumber: // 14
		redisField = "serial-number"
		if isString {
			s.log.Debugf("Received CB Battery Serial Number: %s", valueStr)
			redisValue = valueStr
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoPartNo: // 16
		redisField = "part-number"
		if isInt {
			switch valueInt {
			case 5:
				valueStr = "MAX17301"
			case 6:
				valueStr = "MAX17302"
			case 7:
				valueStr = "MAX17303"
			default:
				valueStr = fmt.Sprintf("MAX1730X (%d)", valueInt) // Include raw value if unknown
			}
			redisValue = valueStr
			isString = true // Mark as string for writing
			isInt = false
			s.log.Debugf("Received CB Battery Part Number: %s (from int %d)", valueStr, valueInt)
		} else if isString {
			// Part number received as string, use directly
			redisValue = valueStr
			s.log.Debugf("Received CB Battery Part Number: %s", valueStr)
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoPresent: // 17
		redisField = "present"
		if isInt {
			presentBool := valueInt != 0
			s.log.Debugf("Received CB Battery Present: %t", presentBool)
			redisValue = presentBool // Store bool type
			isBool = true            // Mark as bool for Redis write switch
			isInt = false            // Unmark as int
		} else {
			processedStandard = false
		}
	case ble.TypeBatteryInfoChargeStatus: // 18
		redisField = "charge-status"
		if isInt {
			switch valueInt {
			case 0:
				valueStr = "not-charging"
			case 1:
				valueStr = "charging"
			default:
				valueStr = "unknown"
			}
			redisValue = valueStr
			isString = true // Mark as string for writing
			isInt = false
			s.log.Debugf("Received CB Battery Charge Status: %s (from int %d)", valueStr, valueInt)
		} else {
			processedStandard = false
		}
	default:
		s.log.Warnf("Unknown or already handled CB Battery Info relative subtype: %d", subType)
		processedStandard = false
	}

	// Write standard types to Redis only if processed and field name is set
	if processedStandard && redisField != "" {
		var err error
		if isInt {
				err = s.ipc.Hash(redisKey).Set(redisField, fmt.Sprintf("%d", valueInt))
		} else if isString {
				err = s.ipc.Hash(redisKey).Set(redisField, valueStr)
		} else if isBool {
			// Convert bool to "true"/"false" string for Redis consistency
			strVal := "false"
			if redisValue.(bool) {
				strVal = "true"
			}
				err = s.ipc.Hash(redisKey).Set(redisField, strVal)
		} // No else needed, already checked processedStandard

		if err != nil {
			s.log.Errorf("Failed to write %s/%s to Redis: %v", redisKey, redisField, err)
		}
	} else if !processedStandard && redisField != "" { // Log if processing failed for a known standard type
		s.log.Warnf("Could not process value for standard CB Battery subtype %d (Field: %s), Value Type: %T", subType, redisField, value)
	}
}

// handleEventMessage handles generic event messages (Type 0x0000) from the nRF.
// These messages contain strings like "topic:payload" (e.g., "scooter:seatbox open").
func (s *Service) handleEventMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	eventStr, ok := value.(string)
	if !ok {
		s.log.Debugf("Handling Event message: value is not a string: %T", value)
		return
	}
	s.log.Debugf("Received event string: %s", eventStr)

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
		// Check if it's a navigation start event
		if strings.HasPrefix(eventStr, "navi:start ") {
			coords := strings.TrimPrefix(eventStr, "navi:start ")
			// Use WriteAndPublishString to atomically HSET and PUBLISH
				err := s.ipc.Hash("navigation").Set("destination", coords)
			if err != nil {
				s.log.Errorf("Failed to set navigation destination: %v", err)
			} else {
				s.log.Debugf("Set navigation destination: %s", coords)
			}
			return
		}

		// Check if it's an APN configuration event
		if strings.HasPrefix(eventStr, "apn ") {
			apnValue := strings.TrimPrefix(eventStr, "apn ")
			// Use WriteAndPublishString to atomically HSET and PUBLISH
				err := s.ipc.Hash("settings").Set("cellular.apn", apnValue)
			if err != nil {
				s.log.Errorf("Failed to set cellular APN: %v", err)
			} else {
				s.log.Debugf("Set cellular APN: %s", apnValue)
			}
			return
		}

		// Check if it's a USB mode event
		if eventStr == "usb:ums" {
			// Set USB mode to UMS and publish
				err := s.ipc.Hash("usb").Set("mode", "ums")
			if err != nil {
				s.log.Errorf("Failed to set USB mode to UMS: %v", err)
			} else {
				s.log.Debugf("Set USB mode: ums")
			}
			return
		}

		if eventStr == "usb:normal" {
			// Set USB mode to normal and publish
				err := s.ipc.Hash("usb").Set("mode", "normal")
			if err != nil {
				s.log.Errorf("Failed to set USB mode to normal: %v", err)
			} else {
				s.log.Debugf("Set USB mode: normal")
			}
			return
		}

		s.log.Warnf(" Received unknown event string: %s", eventStr)
		return // Do not push unknown events
	}

	// Perform LPUSH
	err := ipc.SendRequest(s.ipc, listKey, listValue)
	if err != nil {
		s.log.Errorf("Failed to LPUSH event '%s' to Redis list '%s': %v", listValue, listKey, err)
	} else {
		s.log.Debugf("LPUSHed event '%s' to Redis list '%s'", listValue, listKey)
	}
}

// handlePowerMuxMessage handles incoming power mux messages
func (s *Service) handlePowerMuxMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling Power Mux message (Value Type: %T): %v", value, value)

	powerMuxState, ok := convertToInt(value)
	if !ok {
		s.log.Infof("Received Power Mux message with non-integer value: %v", value)
		return
	}

	var selectedInput string
	if powerMuxState == 0 {
		selectedInput = "aux"
	} else {
		selectedInput = "cb"
	}
	s.log.Debugf("Received PowerMux update: selected-input=%s (raw value: %d)", selectedInput, powerMuxState)

	// Publish the string value to Redis
			err := s.ipc.Hash("power-mux").Set("selected-input", selectedInput)
	if err != nil {
		s.log.Errorf("Error publishing PowerMux state to Redis: %v", err)
	}
}

func (s *Service) writeFaultToRedis(key, source string, code int, message, faultType string) {
	field := faultType // Use "alert" or "fault" as the field name directly

	if code == 0xFF && message == "" {
		s.log.Debugf("Clearing Redis field: %s for key %s", field, key)
		if err := s.ipc.Hash(key).Delete(field); err != nil {
			s.log.Errorf("Error clearing Redis field %s for key %s: %v", field, key, err)
		}
	} else {
		s.log.Debugf("Writing Redis field: %s = '%s' for key %s (Code: %d, Source: %s)", field, message, key, code, source)
		if err := s.ipc.Hash(key).Set(field, message); err != nil {
			s.log.Errorf("Error writing Redis field %s for key %s: %v", field, key, err)
		}
	}
}

// handleAccelerometerMessage handles accelerometer wake-up messages from nRF52
func (s *Service) handleAccelerometerMessage(subType ble.SubType, value interface{}) {
	s.log.Debugf("Handling accelerometer message with relative subtype: %v", subType)

	switch subType {
	case ble.TypeAccelerometerWakeUpSuspend:
		s.log.Debugf("Received accelerometer wake-up from suspend mode")
		// Publish to bmx:interrupt channel to wake up alarm-service
			if _, err := s.ipc.Publish("bmx:interrupt", "wake-suspend"); err != nil {
			s.log.Errorf("Failed to publish accelerometer wake interrupt: %v", err)
		}

	case ble.TypeAccelerometerWakeUpHibernation:
		s.log.Debugf("Received accelerometer wake-up from hibernation mode")
		// Publish to bmx:interrupt channel to wake up alarm-service
			if _, err := s.ipc.Publish("bmx:interrupt", "wake-hibernation"); err != nil {
			s.log.Errorf("Failed to publish accelerometer wake interrupt: %v", err)
		}

	default:
		s.log.Warnf("Unknown accelerometer message relative subtype: %v", subType)
	}
}
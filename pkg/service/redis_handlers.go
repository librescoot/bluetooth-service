package service

import (
	"fmt"
	"strconv"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// SubscribeToRedisChannels subscribes to Redis channels for characteristic writes
func (s *Service) SubscribeToRedisChannels() {
	// Create a new stop channel for subscriptions
	s.subMu.Lock()
	s.subStopCh = make(chan struct{})
	stopCh := s.subStopCh
	s.subMu.Unlock()

	// Define channels based on observed subscriptions
	channels := []string{
		KeyVehicle,         // "vehicle"
		KeyBatterySlot0,    // "battery:0"
		KeyBatterySlot1,    // "battery:1"
		KeyPowerManager,    // "power-manager"
		KeyMileage,         // "engine-ecu"
		KeyFirmwareVersion, // "system"
		KeyBLEPairingPin,   // "ble" - Keep for pin removal notification
		KeyNavigation,      // "navigation"
		KeyUSB,             // "usb"
		KeyKeycard,         // "keycard"
	}

	// Ensure only unique keys are subscribed
	processedKeys := make(map[string]bool)
	uniqueChannels := []string{}
	for _, ch := range channels {
		if !processedKeys[ch] {
			uniqueChannels = append(uniqueChannels, ch)
			processedKeys[ch] = true
		}
	}

	for _, channel := range uniqueChannels {
		s.subWg.Add(1)
		go func(chName string) {
			defer s.subWg.Done()

			watcher := s.ipc.NewHashWatcher(chName)

			watcher.OnAny(func(field, value string) error {
				switch chName {
				case KeyVehicle:
					switch field {
					case "state":
						if err := s.UpdateVehicleState(value); err != nil {
							s.log.Errorf("Error sending vehicle state update triggered by Redis: %v", err)
						}
					case "seatbox:lock":
						if err := s.UpdateSeatboxLock(value); err != nil {
							s.log.Errorf("Error sending seatbox lock update triggered by Redis: %v", err)
						}
					case "handlebar:lock-sensor":
						if err := s.UpdateHandlebarLock(value); err != nil {
							s.log.Errorf("Error sending handlebar lock update triggered by Redis: %v", err)
						}
					}

				case KeyBatterySlot0:
					switch field {
					case "state":
						if err := s.UpdateBatteryActiveStatus(0, value); err != nil {
							s.log.Errorf("Error sending battery:0 state update triggered by Redis: %v", err)
						}
					case "present":
						if err := s.UpdateBatteryPresentStatus(0, value); err != nil {
							s.log.Errorf("Error sending battery:0 presence update triggered by Redis: %v", err)
						}
					case "charge":
						if err := s.UpdateBatteryRemainingCharge(0, value); err != nil {
							s.log.Errorf("Error sending battery:0 charge update triggered by Redis: %v", err)
						}
					case "cycle-count":
						if err := s.UpdateBatteryCycleCount(0, value); err != nil {
							s.log.Errorf("Error sending battery:0 cycle count update triggered by Redis: %v", err)
						}
					}

				case KeyBatterySlot1:
					switch field {
					case "state":
						if err := s.UpdateBatteryActiveStatus(1, value); err != nil {
							s.log.Errorf("Error sending battery:1 state update triggered by Redis: %v", err)
						}
					case "present":
						if err := s.UpdateBatteryPresentStatus(1, value); err != nil {
							s.log.Errorf("Error sending battery:1 presence update triggered by Redis: %v", err)
						}
					case "charge":
						if err := s.UpdateBatteryRemainingCharge(1, value); err != nil {
							s.log.Errorf("Error sending battery:1 charge update triggered by Redis: %v", err)
						}
					case "cycle-count":
						if err := s.UpdateBatteryCycleCount(1, value); err != nil {
							s.log.Errorf("Error sending battery:1 cycle count update triggered by Redis: %v", err)
						}
					}

				case KeyPowerManager:
					if field == "state" {
						if err := s.UpdatePowerManagementState(value); err != nil {
							s.log.Errorf("Error sending power management state update triggered by Redis: %v", err)
						}
					}

				case KeyMileage:
					if field == "odometer" {
						if err := s.UpdateMileage(value); err != nil {
							s.log.Errorf("Error sending mileage update triggered by Redis: %v", err)
						}
					}

				case KeyFirmwareVersion:
					if field == "mdb-version" {
						if err := s.UpdateFirmwareVersion(value); err != nil {
							s.log.Errorf("Error sending firmware version update triggered by Redis: %v", err)
						}
					}

				case KeyBLEPairingPin:
					if field == "pin-code" && value == "" {
						if err := writeUARTMessage(s.usock, ble.TypeBLEPairingPinRemove, 0, 1); err != nil {
							s.log.Errorf("Error sending pairing pin removal command: %v", err)
						}
					}

				case KeyNavigation:
					if field == "destination" || field == "latitude" {
						active := uint16(0)
						if value != "" {
							active = 1
						}
						if err := writeUARTMessage(s.usock, ble.TypeScooterInfo, ble.TypeNavigationActive, active); err != nil {
							s.log.Errorf("Error sending navigation active update: %v", err)
						}
					}

				case KeyUSB:
					if field == "mode" {
						var mode uint16
						if value == "ums" {
							mode = 1
						}
						if err := writeUARTMessage(s.usock, ble.TypeScooterInfo, ble.TypeUMSStatus, mode); err != nil {
							s.log.Errorf("Error sending UMS status update: %v", err)
						}
					}

				case KeyKeycard:
					if field == "command-result" && value != "" {
						s.sendExtendedResponse("keycard:" + value)
					}

				default:
					s.log.Warnf("Unhandled Redis channel in subscription: %s", chName)
				}

				return nil
			})

			watcher.StartWithSync() // Fetches initial state and calls handlers
			defer watcher.Stop()

			<-stopCh
			s.log.Debugf("Stopping subscription for channel %s", chName)
		}(channel) // Pass channel name to the goroutine
	}

	s.log.Infof("Subscribed to Redis channels") // Log after setting up all subscriptions
}

// WatchRedisCommands listens for commands on a Redis list and sends them to the nRF52
func (s *Service) WatchRedisCommands() {
	s.log.Infof("Starting Redis command watcher on list key: %s", KeyBLECommandList)

	handler := ipc.HandleRequests(s.ipc, KeyBLECommandList, func(command string) error {
		s.log.Infof("Received command from Redis list %s: %s", KeyBLECommandList, command)

		// Handle firmware-update command separately
		if command == "firmware-update" {
			s.log.Infof("Received firmware-update command")
			fu := s.GetFirmwareUpdater()
			if fu == nil {
				s.log.Errorf("Firmware updater not configured")
				return nil
			}

			// Get the FirmwareUpdater concrete type to access PerformUpdate
			updater, ok := fu.(*FirmwareUpdater)
			if !ok {
				s.log.Errorf("Firmware updater has unexpected type")
				return nil
			}

			// Get latest firmware
			latestVersion, firmwarePath, err := updater.GetLatestFirmware()
			if err != nil {
				s.log.Errorf("Failed to get latest firmware: %v", err)
				return nil
			}
			if latestVersion == nil {
				s.log.Infof("No firmware files found for force update")
				return nil
			}

			s.log.Infof("Force updating to firmware %s", latestVersion)

			// Perform update in goroutine with timeout
			go func() {
				if err := updater.PerformUpdate(firmwarePath); err != nil {
					s.log.Errorf("Force firmware update failed: %v", err)
				} else {
					s.log.Infof("Force firmware update completed successfully")
				}
			}()
			return nil
		}

		var msgType ble.MessageType
		var subType ble.SubType
		var valueInt uint16 = 0

		switch command {
		case "advertising-start-with-whitelisting":
			msgType = ble.TypeBLECommand
			subType = ble.SubType(ble.BLECommandAdvStartWithWhitelist)
		case "advertising-restart-no-whitelisting":
			msgType = ble.TypeBLECommand
			subType = ble.SubType(ble.BLECommandAdvRestartNoWhitelist)
		case "advertising-stop":
			msgType = ble.TypeBLECommand
			subType = ble.SubType(ble.BLECommandAdvStop)
		case "delete-bond":
			msgType = ble.TypeBLECommand
			subType = ble.SubType(ble.BLECommandDeleteBond)
		case "delete-all-bonds":
			msgType = ble.TypeBLECommand
			subType = ble.SubType(ble.BLECommandDeleteAllBonds)
		case "remove":
			msgType = ble.TypeBLEPairingPinRemove
			subType = 0
			valueInt = 1
			s.log.Debugf("Mapping list command 'remove' to TypeBLEPairingPinRemove")
		case "ltc-enable":
			msgType = ble.TypeLTCControl
			subType = ble.TypeLTCControlSet
			valueInt = 1
		case "ltc-disable":
			msgType = ble.TypeLTCControl
			subType = ble.TypeLTCControlSet
			valueInt = 0
		case "ltc-force-enable":
			msgType = ble.TypeLTCControl
			subType = ble.TypeLTCControlForceSet
			valueInt = 1
		case "ltc-force-disable":
			msgType = ble.TypeLTCControl
			subType = ble.TypeLTCControlForceSet
			valueInt = 0
		case "ltc-status":
			msgType = ble.TypeLTCControl
			subType = ble.TypeLTCControlStatus
			valueInt = 0
		default:
			s.log.Warnf("Unknown command received from Redis list: %s", command)
			return nil // Not an error, just unknown command
		}

		if err := writeUARTMessage(s.usock, msgType, subType, valueInt); err != nil {
			s.log.Errorf("Failed to send command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF: %v", command, msgType, subType, err)
			return err
		}
		s.log.Infof("Sent command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF", command, msgType, subType)
		return nil
	})
	defer handler.Stop()

	<-s.stopCh
	s.log.Infof("Stopping Redis command watcher.")
}

// UpdateVehicleState sends the current vehicle state to nRF52
func (s *Service) UpdateVehicleState(stateStr string) error {
	if stateStr == "" {
		s.log.Warnf(" empty vehicle state value. Sending default (stand-by).")
		stateStr = "stand-by" // Default if empty
	}

	state := vehicleStateToInt(stateStr)

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateState, uint16(state)); err != nil {
		return fmt.Errorf("failed to send vehicle state: %v", err)
	}
	s.log.Infof("Sent vehicle state: %d (from %s)", state, stateStr)
	return nil
}

// UpdateSeatboxLock sends the current seatbox lock state to nRF52
func (s *Service) UpdateSeatboxLock(stateStr string) error {
	state := 0
	// Convert state string to int: "closed"=0, "open"=1
	switch stateStr {
	case "open":
		state = 1
	case "closed":
		state = 0
	default:
		// Try parsing as int
		if val, parseErr := strconv.Atoi(stateStr); parseErr == nil {
			state = val
		}
	}

	stateDesc := "locked"
	if state != 0 {
		stateDesc = "open"
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateSeatbox, uint16(state)); err != nil {
		return fmt.Errorf("failed to send seatbox lock state: %v", err)
	}
	s.log.Infof("Sent seatbox lock state: %d (%s)", state, stateDesc)
	return nil
}

// UpdateHandlebarLock sends the current handlebar lock state to nRF52
func (s *Service) UpdateHandlebarLock(stateStr string) error {
	var stateInt uint16 = 0 // Default to 0 (locked)

	switch stateStr {
	case "locked":
		stateInt = 0
	case "unlocked":
		stateInt = 1
	case "":
		s.log.Warnf(" empty handlebar lock state. Sending default (locked).")
		stateStr = "locked"
		stateInt = 0
	default:
		s.log.Warnf(" unknown handlebar lock state string: '%s'. Sending default (locked).", stateStr)
		stateStr = "locked"
		stateInt = 0
	}

	// Pass the relative subtype and the converted integer state
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateHandlebar, stateInt); err != nil {
		return fmt.Errorf("failed to send handlebar lock state: %v", err)
	}
	s.log.Infof("Sent handlebar lock state: %d (%s)", stateInt, stateStr)
	return nil
}

// UpdateMileage sends the current mileage to nRF52
func (s *Service) UpdateMileage(mileageStr string) error {
	s.mu.Lock()
	if mileageStr == s.lastMileage {
		s.mu.Unlock()
		return nil
	}
	s.lastMileage = mileageStr
	s.mu.Unlock()

	if mileageStr == "" {
		return nil
	}
	mileage, err := strconv.Atoi(mileageStr)
	if err != nil {
		s.log.Warnf("failed to parse mileage value %q: %v", mileageStr, err)
		return nil
	}
	// Pass the relative subtype using 32-bit integer message to match nRF's int32_t
	if err := writeUARTMessage32(s.usock, ble.TypeScooterInfo, ble.TypeMileage, int32(mileage)); err != nil {
		return fmt.Errorf("failed to send mileage: %v", err)
	}
	s.log.Debugf("Sent mileage: %d", mileage)
	return nil
}

// UpdateFirmwareVersion sends the current firmware version to nRF52
func (s *Service) UpdateFirmwareVersion(version string) error {
	// Pass the relative subtype
	if err := writeUARTMessageString(s.usock, ble.TypeScooterInfo, ble.TypeSoftwareVersion, version); err != nil {
		return fmt.Errorf("failed to send firmware version: %v", err)
	}
	s.log.Infof("Sent firmware version: %s", version)
	return nil
}

// UpdateBatteryActiveStatus sends the battery active status to nRF52
func (s *Service) UpdateBatteryActiveStatus(slot int, stateStr string) error {
	var baseSubType ble.SubType = ble.TypeBatterySlot0State
	if slot == 1 {
		baseSubType = ble.TypeBatterySlot1State
	}

	if stateStr == "" {
		stateStr = "unknown"
	}

	status := batteryStateToInt(stateStr)

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(status)); err != nil {
		return fmt.Errorf("failed to send battery:%d status: %v", slot, err)
	}
	s.log.Infof("Set battery:%d state to %s", slot, stateStr)
	return nil
}

// UpdateBatteryPresentStatus sends the battery presence status to nRF52
func (s *Service) UpdateBatteryPresentStatus(slot int, presentStr string) error {
	var baseSubType ble.SubType = ble.TypeBatterySlot0Presence
	if slot == 1 {
		baseSubType = ble.TypeBatterySlot1Presence
	}

	present := 0
	// Try parsing as int first, then check string values
	if val, parseErr := strconv.Atoi(presentStr); parseErr == nil {
		present = val
	} else {
		switch presentStr {
		case "true", "1":
			present = 1
		default:
			present = 0
		}
	}

	presentDesc := "not present"
	if present != 0 {
		presentDesc = "present"
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(present)); err != nil {
		return fmt.Errorf("failed to send battery:%d presence: %v", slot, err)
	}
	s.log.Infof("Set battery:%d present to %s", slot, presentDesc)
	return nil
}

// UpdateBatteryCycleCount sends the battery cycle count to nRF52
func (s *Service) UpdateBatteryCycleCount(slot int, cyclesStr string) error {
	var baseSubType ble.SubType = ble.TypeBatterySlot0CycleCount
	if slot == 1 {
		baseSubType = ble.TypeBatterySlot1CycleCount
	}

	if cyclesStr == "" {
		return nil
	}
	cycles, err := strconv.Atoi(cyclesStr)
	if err != nil {
		s.log.Warnf("failed to parse battery:%d cycle count value %q: %v", slot, cyclesStr, err)
		return nil
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(cycles)); err != nil {
		return fmt.Errorf("failed to send battery:%d cycle-count: %v", slot, err)
	}
	s.log.Infof("Set battery:%d cycle-count to %d", slot, cycles)
	return nil
}

// UpdateBatteryRemainingCharge sends the battery remaining charge to nRF52
func (s *Service) UpdateBatteryRemainingCharge(slot int, chargeStr string) error {
	var baseSubType ble.SubType = ble.TypeBatterySlot0Charge
	if slot == 1 {
		baseSubType = ble.TypeBatterySlot1Charge
	}

	if chargeStr == "" {
		return nil
	}
	charge, err := strconv.Atoi(chargeStr)
	if err != nil {
		s.log.Warnf("failed to parse battery:%d charge value %q: %v", slot, chargeStr, err)
		return nil
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(charge)); err != nil {
		return fmt.Errorf("failed to send battery:%d charge: %v", slot, err)
	}
	s.log.Infof("Set battery:%d charge to %d%%", slot, charge)
	return nil
}

// UpdatePowerManagementState sends the power management state to nRF52
func (s *Service) UpdatePowerManagementState(stateStr string) error {
	if stateStr == "" {
		s.log.Warnf(" empty power management state. Sending default (running).")
		stateStr = "running"
	}

	var stateInt uint16
	switch stateStr {
	case "running":
		stateInt = 1
	case "suspending":
		stateInt = 0
	case "hibernating":
		stateInt = 2
	case "hibernating-manual":
		stateInt = 2
		s.log.Debugf(" hibernating-manual state detected, sending as hibernating (2) to nRF.")
	case "hibernating-timer":
		stateInt = 2
		s.log.Debugf(" hibernating-timer state detected, sending as hibernating (2) to nRF.")
	case "hibernating-l2":
		stateInt = 2 // Send base state
	case "suspending-imminent":
		stateInt = 3
	case "hibernating-imminent":
		stateInt = 4
	case "hibernating-manual-imminent":
		stateInt = 4
		s.log.Debugf(" hibernating-manual-imminent state detected, sending as hibernating-imminent (4) to nRF.")
	case "hibernating-timer-imminent":
		stateInt = 4
		s.log.Debugf(" hibernating-timer-imminent state detected, sending as hibernating-imminent (4) to nRF.")
	case "reboot":
		stateInt = 5
	case "reboot-imminent":
		stateInt = 1
		s.log.Debugf(" Reboot-imminent state detected, sending 'running' state to nRF.")
	case "suspending-pending":
		stateInt = 3
		s.log.Debugf(" %s state detected, sending suspending-imminent to nRF for user feedback.", stateStr)
	case "hibernating-pending", "hibernating-manual-pending", "hibernating-timer-pending":
		stateInt = 4
		s.log.Debugf(" %s state detected, sending hibernating-imminent to nRF for user feedback.", stateStr)
	case "reboot-pending":
		stateInt = 1
		s.log.Debugf(" %s state detected, sending running to nRF.", stateStr)
	default:
		s.log.Warnf("Unknown power management state string: %s. Sending default (running).", stateStr)
		stateInt = 1
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypePowerManagement, ble.TypePowerManagementState, stateInt); err != nil {
		return fmt.Errorf("failed to send power management state: %v", err)
	}
	s.log.Infof("Sent power management state: %d (from %s)", stateInt, stateStr)

	// Handle hibernation level separately if needed
	if stateStr == "hibernating-l2" {
		level := uint16(ble.HibernationLevelL2)
		// Pass the relative subtype
		if err := writeUARTMessage(s.usock, ble.TypePowerManagement, ble.TypePowerManagementPowerRequest, level); err != nil {
			s.log.Warnf(" failed to send power management level L2 request: %v", err)
		} else {
			s.log.Infof("Sent power management hibernation level request: L2")
		}
	}

	// Handle hibernation commands to nRF chip
	if stateStr == "hibernating" || stateStr == "hibernating-manual" || stateStr == "hibernating-timer" {
		// Disable data streaming before hibernation to prevent automatic wake-up
		if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
			s.log.Warnf(" failed to disable data stream before hibernation: %v", err)
		} else {
			s.log.Infof("Disabled data stream before hibernation")
		}

		// Send hibernation level request (default to L1)
		hibernationLevel := uint16(ble.HibernationLevelL1)
		if err := writeUARTMessage(s.usock, ble.TypePowerManagement, ble.TypePowerManagementPowerRequest, hibernationLevel); err != nil {
			s.log.Warnf(" failed to send hibernation level request to nRF: %v", err)
		} else {
			s.log.Infof("Sent hibernation level request to nRF: L%d", hibernationLevel+1)
		}
	}

	return nil
}

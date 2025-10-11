package service

import (
	"fmt"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/redis/go-redis/v9"
)

// SubscribeToRedisChannels subscribes to Redis channels for characteristic writes
func (s *Service) SubscribeToRedisChannels() {
	// Define channels based on observed subscriptions
	channels := []string{
		KeyVehicle,         // "vehicle"
		KeyBatterySlot0,    // "battery:0"
		KeyBatterySlot1,    // "battery:1"
		KeyPowerManager,    // "power-manager"
		KeyMileage,         // "engine-ecu"
		KeyFirmwareVersion, // "system"
		KeyBLEPairingPin,   // "ble" - Keep for pin removal notification
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
		s.wg.Add(1)
		go func(chName string) {
			defer s.wg.Done()
			pubsub, closeFunc := s.redis.Subscribe(chName)
			defer closeFunc()

			for {
				select {
				case <-s.stopCh:
					s.log.Debugf("Stopping subscription for channel %s", chName)
					return
				case msg, ok := <-pubsub:
					if !ok {
						s.log.Debugf("Subscription channel %s closed", chName)
						return
					}
					s.log.Debugf("Received Redis message on channel %s: %s", chName, msg.Payload)
					field := msg.Payload // Payload is the field name that changed

					switch chName {
					case KeyVehicle:
						switch field {
						case "state":
							if err := s.UpdateVehicleState(); err != nil {
								s.log.Errorf("Error sending vehicle state update triggered by Redis: %v", err)
							}
						case "seatbox:lock":
							if err := s.UpdateSeatboxLock(); err != nil {
								s.log.Errorf("Error sending seatbox lock update triggered by Redis: %v", err)
							}
						case "handlebar:lock-sensor":
							if err := s.UpdateHandlebarLock(); err != nil {
								s.log.Errorf("Error sending handlebar lock update triggered by Redis: %v", err)
							}
						default:
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyBatterySlot0:
						switch field {
						case "state":
							if err := s.UpdateBatteryActiveStatus(0); err != nil {
								s.log.Errorf("Error sending battery:0 state update triggered by Redis: %v", err)
							}
						case "present":
							if err := s.UpdateBatteryPresentStatus(0); err != nil {
								s.log.Errorf("Error sending battery:0 presence update triggered by Redis: %v", err)
							}
							if err := s.UpdateBatteryCycleCount(0); err != nil {
								s.log.Errorf("Error sending battery:0 cycle count update triggered by Redis: %v", err)
							}
						case "charge":
							if err := s.UpdateBatteryRemainingCharge(0); err != nil {
								s.log.Errorf("Error sending battery:0 charge update triggered by Redis: %v", err)
							}
						case "cycle-count":
							if err := s.UpdateBatteryCycleCount(0); err != nil {
								s.log.Errorf("Error sending battery:0 cycle count update triggered by Redis: %v", err)
							}
						case "temperature-state":
							// Silently ignore temperature-state updates
							s.log.Debugf("Ignoring temperature-state update for battery:0")
						default:
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyBatterySlot1:
						switch field {
						case "state":
							if err := s.UpdateBatteryActiveStatus(1); err != nil {
								s.log.Errorf("Error sending battery:1 state update triggered by Redis: %v", err)
							}
						case "present":
							if err := s.UpdateBatteryPresentStatus(1); err != nil {
								s.log.Errorf("Error sending battery:1 presence update triggered by Redis: %v", err)
							}
							if err := s.UpdateBatteryCycleCount(1); err != nil {
								s.log.Errorf("Error sending battery:1 cycle count update triggered by Redis: %v", err)
							}
						case "charge":
							if err := s.UpdateBatteryRemainingCharge(1); err != nil {
								s.log.Errorf("Error sending battery:1 charge update triggered by Redis: %v", err)
							}
						case "cycle-count":
							if err := s.UpdateBatteryCycleCount(1); err != nil {
								s.log.Errorf("Error sending battery:1 cycle count update triggered by Redis: %v", err)
							}
						case "temperature-state":
							// Silently ignore temperature-state updates
							s.log.Debugf("Ignoring temperature-state update for battery:1")
						default:
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyPowerManager:
						if field == "state" {
							if err := s.UpdatePowerManagementState(); err != nil {
								s.log.Errorf("Error sending power management state update triggered by Redis: %v", err)
							}
						} else {
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyMileage:
						if field == "odometer" {
							if err := s.UpdateMileage(); err != nil {
								s.log.Errorf("Error sending mileage update triggered by Redis: %v", err)
							}
						} else {
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyFirmwareVersion:
						if field == "mdb-version" {
							if err := s.UpdateFirmwareVersion(); err != nil {
								s.log.Errorf("Error sending firmware version update triggered by Redis: %v", err)
							}
						} else {
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					case KeyBLEPairingPin:
						if field == "pin-code" {
							pin, err := s.redis.GetString(KeyBLEPairingPin, "pin-code")
							if (err != nil && err != redis.Nil) || pin == "" {
								s.log.Debugf("Pin code removed notification received for channel '%s'. Sending removal command.", chName)
								if err := writeUARTMessage(s.usock, ble.TypeBLEPairingPinRemove, 0, 1); err != nil {
									s.log.Errorf("Error sending pairing pin removal command: %v", err)
								}
							} else {
								s.log.Debugf("Pin code set/updated notification received for channel '%s'. No action needed.", chName)
							}
						} else {
							s.log.Warnf("Unhandled field '%s' for channel '%s'", field, chName)
						}

					default:
						s.log.Warnf("Unhandled Redis channel in subscription: %s", chName)
					}
				}
			}
		}(channel) // Pass channel name to the goroutine
	}

	s.log.Infof("Subscribed to Redis channels") // Log after setting up all subscriptions
}

// WatchRedisCommands listens for commands on a Redis list (using BRPOP)
// and sends the corresponding command to the nRF52.
func (s *Service) WatchRedisCommands() {
	s.wg.Add(1)
	defer s.wg.Done()
	s.log.Infof("Starting Redis command watcher on list key: %s", KeyBLECommandList)
	for {
		select {
		case <-s.stopCh: // Check if service is stopping
			s.log.Infof("Stopping Redis command watcher.")
			return
		default:
			// Use 1 second timeout to allow periodic stopCh checks
			result, err := s.redis.BRPop(1*time.Second, KeyBLECommandList)
			if err != nil {
				// Don't log Nil errors, they just mean timeout
				if err != redis.Nil {
					s.log.Errorf("Error receiving command from Redis list %s: %v", KeyBLECommandList, err)
					time.Sleep(1 * time.Second)
				}
				continue // Continue loop to retry BRPOP
			}

			// result should be [listKey, commandString]
			// Empty result means timeout, just continue
			if len(result) == 0 {
				continue
			}

			if len(result) != 2 {
				s.log.Warnf("Unexpected BRPOP result format: %v", result)
				continue
			}

			command := result[1] // The actual command string
			s.log.Infof("Received command from Redis list %s: %s", KeyBLECommandList, command)

			var msgType ble.MessageType
			var subType ble.SubType
			var valueInt uint16 = 0 // Most commands have 0 value
			sendCmd := true

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
				subType = 0  // No specific subtype needed
				valueInt = 1 // Value doesn't matter, use 1
				s.log.Debugf("Mapping list command 'remove' to TypeBLEPairingPinRemove")
			default:
				s.log.Warnf("Unknown command received from Redis list: %s", command)
				sendCmd = false
			}

			if sendCmd {
				if err := writeUARTMessage(s.usock, msgType, subType, valueInt); err != nil {
					s.log.Errorf("Failed to send command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF: %v", command, msgType, subType, err)
				} else {
					s.log.Infof("Sent command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF", command, msgType, subType)
				}
			}
		}
	}
}

// UpdateVehicleState sends the current vehicle state from Redis to nRF52
func (s *Service) UpdateVehicleState() error {
	stateStr, err := s.redis.GetString(KeyVehicle, "state")
	if err != nil {
		s.log.Warnf(" failed to get vehicle state from Redis: %v. Sending default (stand-by).", err)
		stateStr = "stand-by" // Default if not found
	}

	state := vehicleStateToInt(stateStr)

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateState, uint16(state)); err != nil {
		return fmt.Errorf("failed to send vehicle state: %v", err)
	}
	s.log.Infof("Sent vehicle state: %d (from %s)", state, stateStr)
	return nil
}

// UpdateSeatboxLock sends the current seatbox lock state from Redis to nRF52
func (s *Service) UpdateSeatboxLock() error {
	state, err := s.redis.GetStateInt(KeyVehicle, "seatbox:lock")
	if err != nil {
		s.log.Warnf(" failed to get seatbox lock state from Redis: %v. Sending default (0).", err)
		state = 0 // Default if not found
	}

	stateStr := "locked"
	if state != 0 {
		stateStr = "open"
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateSeatbox, uint16(state)); err != nil {
		return fmt.Errorf("failed to send seatbox lock state: %v", err)
	}
	s.log.Infof("Sent seatbox lock state: %d (%s)", state, stateStr)
	return nil
}

// UpdateHandlebarLock sends the current handlebar lock state from Redis to nRF52
func (s *Service) UpdateHandlebarLock() error {
	stateStr, err := s.redis.GetString(KeyVehicle, "handlebar:lock-sensor") // Read as string first
	var stateInt uint16 = 0                                                 // Default to 0 (locked?)

	if err != nil {
		s.log.Warnf(" failed to get handlebar lock state from Redis: %v. Sending default (locked).", err)
		stateStr = "locked"
		// stateInt remains 0
	} else {
		switch stateStr {
		case "locked":
			stateInt = 0
		case "unlocked":
			stateInt = 1
		default:
			s.log.Warnf(" unknown handlebar lock state string from Redis: '%s'. Sending default (locked).", stateStr)
			stateStr = "locked"
			stateInt = 0
		}
	}

	// Pass the relative subtype and the converted integer state
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateHandlebar, stateInt); err != nil {
		return fmt.Errorf("failed to send handlebar lock state: %v", err)
	}
	s.log.Infof("Sent handlebar lock state: %d (%s)", stateInt, stateStr)
	return nil
}

// UpdateMileage sends the current mileage from Redis to nRF52
func (s *Service) UpdateMileage() error {
	mileage, err := s.redis.GetInt(KeyMileage, "odometer")
	if err != nil {
		s.log.Warnf(" failed to get mileage from Redis: %v. Sending 0.", err)
		mileage = 0 // Default if not found
	}
	// Pass the relative subtype using 32-bit integer message to match nRF's int32_t
	if err := writeUARTMessage32(s.usock, ble.TypeScooterInfo, ble.TypeMileage, int32(mileage)); err != nil {
		return fmt.Errorf("failed to send mileage: %v", err)
	}
	s.log.Infof("Sent mileage: %d", mileage)
	return nil
}

// UpdateFirmwareVersion sends the current firmware version from Redis to nRF52
func (s *Service) UpdateFirmwareVersion() error {
	version, err := s.redis.GetString(KeyFirmwareVersion, "mdb-version")
	if err != nil {
		s.log.Warnf(" failed to get firmware version from Redis: %v. Sending empty string.", err)
		version = "" // Default if not found
	}
	// Pass the relative subtype
	if err := writeUARTMessageString(s.usock, ble.TypeScooterInfo, ble.TypeSoftwareVersion, version); err != nil {
		return fmt.Errorf("failed to send firmware version: %v", err)
	}
	s.log.Infof("Sent firmware version: %s", version)
	return nil
}

// UpdateBatteryActiveStatus sends the battery active status from Redis to nRF52
func (s *Service) UpdateBatteryActiveStatus(slot int) error {
	key := KeyBatterySlot0
	var baseSubType ble.SubType = ble.TypeBatterySlot0State
	if slot == 1 {
		key = KeyBatterySlot1
		baseSubType = ble.TypeBatterySlot1State
	}

	stateStr, err := s.redis.GetString(key, "state")
	if err != nil {
		s.log.Warnf(" failed to get battery:%d status from Redis: %v. Sending default.", slot, err)
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

// UpdateBatteryPresentStatus sends the battery presence status from Redis to nRF52
func (s *Service) UpdateBatteryPresentStatus(slot int) error {
	key := KeyBatterySlot0
	var baseSubType ble.SubType = ble.TypeBatterySlot0Presence
	if slot == 1 {
		key = KeyBatterySlot1
		baseSubType = ble.TypeBatterySlot1Presence
	}

	present, err := s.redis.GetInt(key, "present")
	if err != nil {
		presentStr, strErr := s.redis.GetString(key, "present")
		if strErr != nil {
			s.log.Warnf(" Failed to get battery:%d presence from Redis: %v. Sending default (not present).", slot, strErr)
			present = 0
		} else {
			switch presentStr {
			case "true", "1":
				present = 1
			default:
				present = 0
			}
		}
	}

	presentStr := "not present"
	if present != 0 {
		presentStr = "present"
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(present)); err != nil {
		return fmt.Errorf("failed to send battery:%d presence: %v", slot, err)
	}
	s.log.Infof("Set battery:%d present to %s", slot, presentStr)
	return nil
}

// UpdateBatteryCycleCount sends the battery cycle count from Redis to nRF52
func (s *Service) UpdateBatteryCycleCount(slot int) error {
	key := KeyBatterySlot0
	var baseSubType ble.SubType = ble.TypeBatterySlot0CycleCount
	if slot == 1 {
		key = KeyBatterySlot1
		baseSubType = ble.TypeBatterySlot1CycleCount
	}

	cycles, err := s.redis.GetInt(key, "cycle-count")
	if err != nil {
		s.log.Warnf(" failed to get battery:%d cycle-count from Redis: %v. Sending 0.", slot, err)
		cycles = 0
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(cycles)); err != nil {
		return fmt.Errorf("failed to send battery:%d cycle-count: %v", slot, err)
	}
	s.log.Infof("Set battery:%d cycle-count to %d", slot, cycles)
	return nil
}

// UpdateBatteryRemainingCharge sends the battery remaining charge from Redis to nRF52
func (s *Service) UpdateBatteryRemainingCharge(slot int) error {
	key := KeyBatterySlot0
	var baseSubType ble.SubType = ble.TypeBatterySlot0Charge
	if slot == 1 {
		key = KeyBatterySlot1
		baseSubType = ble.TypeBatterySlot1Charge
	}

	charge, err := s.redis.GetInt(key, "charge")
	if err != nil {
		s.log.Warnf(" failed to get battery:%d charge from Redis: %v. Sending 0.", slot, err)
		charge = 0
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(charge)); err != nil {
		return fmt.Errorf("failed to send battery:%d charge: %v", slot, err)
	}
	s.log.Infof("Set battery:%d charge to %d%%", slot, charge)
	return nil
}

// UpdatePowerManagementState sends the power management state from Redis to nRF52
func (s *Service) UpdatePowerManagementState() error {
	stateStr, err := s.redis.GetString(KeyPowerManager, "state")
	if err != nil {
		s.log.Warnf(" failed to get power management state from Redis: %v. Sending default (running).", err)
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

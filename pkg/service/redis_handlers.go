package service

import (
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// SubscribeToRedisChannels subscribes to Redis channels for characteristic writes
func (s *Service) SubscribeToRedisChannels() {
	// Define channels based on observed subscriptions
	channels := []string{
		KeyVehicle,           // "vehicle"
		KeyBatterySlot1,      // "battery:0"
		KeyBatterySlot2,      // "battery:1"
			KeyPowerManager,      // "power-manager"
		KeyMileage,           // "engine-ecu"
		KeyFirmwareVersion,   // "system"
			KeyBLEPairingPin,     // "ble" - Keep for pin removal notification
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
		go func(chName string) {
			pubsub, closeFunc := s.redis.Subscribe(chName)
			defer closeFunc()

			for msg := range pubsub {
				log.Printf("Received Redis message on channel %s: %s", chName, msg.Payload)
				field := msg.Payload // Payload is the field name that changed

				switch chName {
				case KeyVehicle:
					switch field {
					case "state":
						if err := s.UpdateVehicleState(); err != nil {
							log.Printf("Error sending vehicle state update triggered by Redis: %v", err)
						}
					case "seatbox:lock":
						if err := s.UpdateSeatboxLock(); err != nil {
							log.Printf("Error sending seatbox lock update triggered by Redis: %v", err)
						}
					case "handlebar:lock-sensor":
						if err := s.UpdateHandlebarLock(); err != nil {
							log.Printf("Error sending handlebar lock update triggered by Redis: %v", err)
						}
					default:
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				case KeyBatterySlot1:
					switch field {
					case "state":
						if err := s.UpdateBatteryActiveStatus(1); err != nil {
							log.Printf("Error sending battery slot 1 state update triggered by Redis: %v", err)
						}
					case "present":
						if err := s.UpdateBatteryPresentStatus(1); err != nil {
							log.Printf("Error sending battery slot 1 presence update triggered by Redis: %v", err)
						}
						if err := s.UpdateBatteryCycleCount(1); err != nil {
							log.Printf("Error sending battery slot 1 cycle count update triggered by Redis: %v", err)
						}
					case "charge":
						if err := s.UpdateBatteryRemainingCharge(1); err != nil {
							log.Printf("Error sending battery slot 1 charge update triggered by Redis: %v", err)
						}
					case "cycle-count":
						if err := s.UpdateBatteryCycleCount(1); err != nil {
							log.Printf("Error sending battery slot 1 cycle count update triggered by Redis: %v", err)
						}
					default:
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				case KeyBatterySlot2:
					switch field {
					case "state":
						if err := s.UpdateBatteryActiveStatus(2); err != nil {
							log.Printf("Error sending battery slot 2 state update triggered by Redis: %v", err)
						}
					case "present":
						if err := s.UpdateBatteryPresentStatus(2); err != nil {
							log.Printf("Error sending battery slot 2 presence update triggered by Redis: %v", err)
						}
						if err := s.UpdateBatteryCycleCount(2); err != nil {
							log.Printf("Error sending battery slot 2 cycle count update triggered by Redis: %v", err)
						}
					case "charge":
						if err := s.UpdateBatteryRemainingCharge(2); err != nil {
							log.Printf("Error sending battery slot 2 charge update triggered by Redis: %v", err)
						}
					case "cycle-count":
						if err := s.UpdateBatteryCycleCount(2); err != nil {
							log.Printf("Error sending battery slot 2 cycle count update triggered by Redis: %v", err)
						}
					default:
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				case KeyPowerManager:
					if field == "state" {
						if err := s.UpdatePowerManagementState(); err != nil {
							log.Printf("Error sending power management state update triggered by Redis: %v", err)
						}
					} else {
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				case KeyMileage:
					if field == "odometer" {
						if err := s.UpdateMileage(); err != nil {
							log.Printf("Error sending mileage update triggered by Redis: %v", err)
						}
					} else {
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				case KeyFirmwareVersion:
					if field == "mdb-version" {
						if err := s.UpdateFirmwareVersion(); err != nil {
							log.Printf("Error sending firmware version update triggered by Redis: %v", err)
						}
					} else {
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}
				
				case KeyBLEPairingPin:
					if field == "pin-code" {
						pin, err := s.redis.GetString(KeyBLEPairingPin, "pin-code")
						if (err != nil && err != redis.Nil) || pin == "" { 
							log.Printf("Pin code removed notification received for channel '%s'. Sending removal command.", chName)
							if err := writeUARTMessage(s.usock, ble.TypeBLEPairingPinRemove, 0, 1); err != nil {
								log.Printf("Error sending pairing pin removal command: %v", err)
							}
						} else {
							log.Printf("Pin code set/updated notification received for channel '%s'. No action needed.", chName)
						}
					} else {
						log.Printf("Unhandled field '%s' for channel '%s'", field, chName)
					}

				default:
					log.Printf("Unhandled Redis channel in subscription: %s", chName)
				}
			}
		}(channel) // Pass channel name to the goroutine
	}

	log.Println("Subscribed to Redis channels") // Log after setting up all subscriptions
}

// WatchRedisCommands listens for commands on a Redis list (using BRPOP)
// and sends the corresponding command to the nRF52.
func (s *Service) WatchRedisCommands() {
	log.Printf("Starting Redis command watcher on list key: %s", KeyBLECommandList)
	for {
		select {
		case <-s.stopCh: // Check if service is stopping
			log.Println("Stopping Redis command watcher.")
			return
		default:
			// Block indefinitely waiting for a command (timeout 0)
			result, err := s.redis.BRPop(0*time.Second, KeyBLECommandList)
			if err != nil {
				// Don't log Nil errors, they just mean timeout (which shouldn't happen with 0)
				if err != redis.Nil {
					log.Printf("Error receiving command from Redis list %s: %v", KeyBLECommandList, err)
					// Optionally add a small delay before retrying after an error
					time.Sleep(1 * time.Second)
				}
				continue // Continue loop to retry BRPOP
			}

			// result should be [listKey, commandString]
			if result == nil || len(result) != 2 {
				log.Printf("Warning: Received nil or unexpected result from BRPOP: %v", result)
				continue
			}

			command := result[1] // The actual command string
			log.Printf("Received command from Redis list %s: %s", KeyBLECommandList, command)

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
				subType = 0 // No specific subtype needed
				valueInt = 1 // Value doesn't matter, use 1
				log.Printf("Mapping list command 'remove' to TypeBLEPairingPinRemove")
			default:
				log.Printf("Unknown command received from Redis list: %s", command)
				sendCmd = false
			}

			if sendCmd {
				if err := writeUARTMessage(s.usock, msgType, subType, valueInt); err != nil {
					log.Printf("Failed to send command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF: %v", command, msgType, subType, err)
				} else {
					log.Printf("Sent command '%s' (Type: 0x%04x, SubType: 0x%04x) to nRF", command, msgType, subType)
				}
			}
		}
	}
}

// UpdateVehicleState sends the current vehicle state from Redis to nRF52
func (s *Service) UpdateVehicleState() error {
	state, err := s.redis.GetStateInt(KeyVehicle, "state")
	if err != nil {
		log.Printf("Warning: failed to get vehicle state from Redis: %v. Sending default (0).", err)
		state = 0 // Default if not found
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateState, uint16(state)); err != nil {
		return fmt.Errorf("failed to send vehicle state: %v", err)
	}
	log.Printf("Sent vehicle state: %d", state)
	return nil
}

// UpdateSeatboxLock sends the current seatbox lock state from Redis to nRF52
func (s *Service) UpdateSeatboxLock() error {
	state, err := s.redis.GetStateInt(KeyVehicle, "seatbox:lock")
	if err != nil {
		log.Printf("Warning: failed to get seatbox lock state from Redis: %v. Sending default (0).", err)
		state = 0 // Default if not found
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateSeatbox, uint16(state)); err != nil {
		return fmt.Errorf("failed to send seatbox lock state: %v", err)
	}
	log.Printf("Sent seatbox lock state: %d", state)
	return nil
}

// UpdateHandlebarLock sends the current handlebar lock state from Redis to nRF52
func (s *Service) UpdateHandlebarLock() error {
	stateStr, err := s.redis.GetString(KeyVehicle, "handlebar:lock-sensor") // Read as string first
	var stateInt uint16 = 0 // Default to 0 (locked?)

	if err != nil {
		log.Printf("Warning: failed to get handlebar lock state from Redis: %v. Sending default (0).", err)
		// stateInt remains 0
	} else {
		switch stateStr {
		case "locked":
			stateInt = 0
		case "unlocked":
			stateInt = 1
		default:
			log.Printf("Warning: unknown handlebar lock state string from Redis: '%s'. Sending default (0).", stateStr)
			stateInt = 0
		}
	}

	// Pass the relative subtype and the converted integer state
	if err := writeUARTMessage(s.usock, ble.TypeVehicleState, ble.TypeVehicleStateHandlebar, stateInt); err != nil {
		return fmt.Errorf("failed to send handlebar lock state: %v", err)
	}
	log.Printf("Sent handlebar lock state: %d", stateInt)
	return nil
}

// UpdateMileage sends the current mileage from Redis to nRF52
func (s *Service) UpdateMileage() error {
	mileage, err := s.redis.GetInt(KeyMileage, "odometer")
	if err != nil {
		log.Printf("Warning: failed to get mileage from Redis: %v. Sending 0.", err)
		mileage = 0 // Default if not found
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeScooterInfo, ble.TypeMileage, uint16(mileage)); err != nil {
		return fmt.Errorf("failed to send mileage: %v", err)
	}
	log.Printf("Sent mileage: %d", mileage)
	return nil
}

// UpdateFirmwareVersion sends the current firmware version from Redis to nRF52
func (s *Service) UpdateFirmwareVersion() error {
	version, err := s.redis.GetString(KeyFirmwareVersion, "mdb-version")
	if err != nil {
		log.Printf("Warning: failed to get firmware version from Redis: %v. Sending empty string.", err)
		version = "" // Default if not found
	}
	// Pass the relative subtype
	if err := writeUARTMessageString(s.usock, ble.TypeScooterInfo, ble.TypeSoftwareVersion, version); err != nil {
		return fmt.Errorf("failed to send firmware version: %v", err)
	}
	log.Printf("Sent firmware version: %s", version)
	return nil
}

// UpdateBatteryActiveStatus sends the battery active status from Redis to nRF52
func (s *Service) UpdateBatteryActiveStatus(slot int) error {
	key := KeyBatterySlot1
	var baseSubType ble.SubType = ble.TypeBatterySlot1State
	if slot == 2 {
		key = KeyBatterySlot2
		baseSubType = ble.TypeBatterySlot2State
	}

	stateStr, err := s.redis.GetString(key, "state")
	if err != nil {
		log.Printf("Warning: failed to get battery status for slot %d from Redis: %v. Sending default.", slot, err)
		stateStr = "unknown"
	}

	status := batteryStateToInt(stateStr)

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(status)); err != nil {
		return fmt.Errorf("failed to send battery status for slot %d: %v", slot, err)
	}
	log.Printf("Sent battery status for slot %d: %d (from %s)", slot, status, stateStr)
	return nil
}

// UpdateBatteryPresentStatus sends the battery presence status from Redis to nRF52
func (s *Service) UpdateBatteryPresentStatus(slot int) error {
	key := KeyBatterySlot1
	var baseSubType ble.SubType = ble.TypeBatterySlot1Presence
	if slot == 2 {
		key = KeyBatterySlot2
		baseSubType = ble.TypeBatterySlot2Presence
	}

	present, err := s.redis.GetInt(key, "present")
	if err != nil {
		presentStr, strErr := s.redis.GetString(key, "present")
		if strErr != nil {
			log.Printf("Warning: Failed to get battery presence status for slot %d from Redis: %v. Sending default (0).", slot, strErr)
			present = 0
		} else {
			switch presentStr {
			case "true", "1": present = 1
			default: present = 0
			}
		}
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(present)); err != nil {
		return fmt.Errorf("failed to send battery presence status for slot %d: %v", slot, err)
	}
	log.Printf("Sent battery presence status for slot %d: %d", slot, present)
	return nil
}

// UpdateBatteryCycleCount sends the battery cycle count from Redis to nRF52
func (s *Service) UpdateBatteryCycleCount(slot int) error {
	key := KeyBatterySlot1
	var baseSubType ble.SubType = ble.TypeBatterySlot1CycleCount
	if slot == 2 {
		key = KeyBatterySlot2
		baseSubType = ble.TypeBatterySlot2CycleCount
	}

	cycles, err := s.redis.GetInt(key, "cycle-count")
	if err != nil {
		log.Printf("Warning: failed to get battery cycle count for slot %d from Redis: %v. Sending 0.", slot, err)
		cycles = 0
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(cycles)); err != nil {
		return fmt.Errorf("failed to send battery cycle count for slot %d: %v", slot, err)
	}
	log.Printf("Sent battery cycle count for slot %d: %d", slot, cycles)
	return nil
}

// UpdateBatteryRemainingCharge sends the battery remaining charge from Redis to nRF52
func (s *Service) UpdateBatteryRemainingCharge(slot int) error {
	key := KeyBatterySlot1
	var baseSubType ble.SubType = ble.TypeBatterySlot1Charge
	if slot == 2 {
		key = KeyBatterySlot2
		baseSubType = ble.TypeBatterySlot2Charge
	}

	charge, err := s.redis.GetInt(key, "charge")
	if err != nil {
		log.Printf("Warning: failed to get battery charge for slot %d from Redis: %v. Sending 0.", slot, err)
		charge = 0
	}
	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypeBattery, baseSubType, uint16(charge)); err != nil {
		return fmt.Errorf("failed to send battery charge for slot %d: %v", slot, err)
	}
	log.Printf("Sent battery charge for slot %d: %d", slot, charge)
	return nil
}

// UpdatePowerManagementState sends the power management state from Redis to nRF52
func (s *Service) UpdatePowerManagementState() error {
	stateStr, err := s.redis.GetString(KeyPowerManager, "state")
	if err != nil {
		log.Printf("Warning: failed to get power management state from Redis: %v. Sending default (running).", err)
		stateStr = "running"
	}

	var stateInt uint16
	switch stateStr {
	case "running": stateInt = 1
	case "suspending": stateInt = 0
	case "hibernating": stateInt = 2
	case "hibernating-l2": stateInt = 2 // Send base state
	case "suspending-imminent": stateInt = 3
	case "hibernating-imminent": stateInt = 4
	case "reboot": stateInt = 5
	case "reboot-imminent": stateInt = 1
		log.Printf("Info: Reboot-imminent state detected, sending 'running' state to nRF.")
	default:
		log.Printf("Unknown power management state string: %s. Sending default (running).", stateStr)
		stateInt = 1
	}

	// Pass the relative subtype
	if err := writeUARTMessage(s.usock, ble.TypePowerManagement, ble.TypePowerManagementState, stateInt); err != nil {
		return fmt.Errorf("failed to send power management state: %v", err)
	}
	log.Printf("Sent power management state: %d (from %s)", stateInt, stateStr)

	// Handle hibernation level separately if needed
	if stateStr == "hibernating-l2" {
		level := uint16(1)
		// Pass the relative subtype
		if err := writeUARTMessage(s.usock, ble.TypePowerManagement, ble.TypePowerManagementPowerRequest, level); err != nil {
			log.Printf("Warning: failed to send power management level L2 request: %v", err)
		} else {
			log.Printf("Sent power management hibernation level request: L2")
		}
	}

	return nil
} 
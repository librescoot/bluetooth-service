package service

import (
	"fmt"
	"log"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// InitializeNRF52 initializes communication with the nRF52
func (s *Service) InitializeNRF52() error {
	log.Println("Starting nRF52 initialization...")

	// 1. Disable data streaming
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		log.Printf("Warning: failed to disable data streaming: %v", err)
	} else {
		log.Println("Sent Disable Data Streaming command")
	}
	time.Sleep(50 * time.Millisecond)

	// 2. Request BLE firmware version
	if err := writeUARTMessage(s.usock, ble.TypeBLEVersion, ble.TypeBLEVersionString, 0); err != nil {
		log.Printf("Warning: failed to request BLE firmware version: %v", err)
	} else {
		log.Println("Sent Request BLE Firmware Version command")
	}
	time.Sleep(50 * time.Millisecond)

	// 3. Request BLE MAC address
	if err := writeUARTMessage(s.usock, ble.TypeBLEParam, ble.TypeBLEParamMACAddress, 0); err != nil {
		log.Printf("Warning: failed to request BLE MAC address: %v", err)
	} else {
		log.Println("Sent Request BLE MAC Address command")
	}
	time.Sleep(50 * time.Millisecond)

	// 4. Enable data streaming
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 1); err != nil {
		log.Printf("Warning: failed to enable data streaming: %v", err)
	} else {
		log.Println("Sent Enable Data Streaming command")
	}
	time.Sleep(50 * time.Millisecond)

	// 5. Sync data stream
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamSync, 1); err != nil {
		log.Printf("Warning: Failed to sync data stream: %v", err)
	} else {
		log.Println("Sent Data Stream Sync command")
	}
	time.Sleep(50 * time.Millisecond)

	// 6. Start advertising (No Whitelist)
	if err := writeUARTMessage(s.usock, ble.TypeBLECommand, ble.SubType(ble.BLECommandAdvRestartNoWhitelist), 0); err != nil {
		log.Printf("Warning: failed to send command to restart advertising without whitelist: %v", err)
	} else {
		log.Println("Sent command to restart advertising without whitelist")
	}

	log.Println("nRF52 basic initialization sequence sent")
	return nil
}


// RestartAdvertisingWithoutWhitelist sends command to restart advertising without whitelist
func (s *Service) RestartAdvertisingWithoutWhitelist() error {
	if err := writeUARTMessage(s.usock, ble.TypeBLECommand, ble.SubType(ble.BLECommandAdvRestartNoWhitelist), 0); err != nil {
		return fmt.Errorf("failed to send advertising restart command: %v", err)
	}
	log.Println("Sent command to restart advertising without whitelist")
	return nil
} 
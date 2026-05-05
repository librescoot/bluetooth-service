package service

import (
	"fmt"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// nrfHandshakeTimeout caps how long InitializeNRF52 waits for the nRF to
// reply to the version request. The reply normally arrives in a few ms over
// USOCK; 2 seconds is generous enough to ride out worst-case scheduling
// latency and tight enough to surface a non-responsive nRF immediately
// instead of leaving the service silently degraded.
const nrfHandshakeTimeout = 2 * time.Second

// InitializeNRF52 initializes communication with the nRF52 and blocks until
// the nRF acknowledges by emitting a BLE version frame, or until
// nrfHandshakeTimeout elapses. A timeout is returned as an error so callers
// (Bootstrap.startNormally, ReconnectUSock) can surface FaultNRFInit or
// failed:reconnect as appropriate. Late-arriving versions clear the fault
// via handleBLEVersionMessage, so a slow handshake self-heals.
func (s *Service) InitializeNRF52() error {
	s.log.Infof("Starting nRF52 initialization...")

	// Drain any stale version signal from a previous handshake. The buffered
	// channel may still hold an entry if the version handler fired between
	// the previous wait and now (e.g. on rapid reconnects).
	select {
	case <-s.versionRxCh:
	default:
	}

	// 1. Disable data streaming
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		s.log.Warnf(" failed to disable data streaming: %v", err)
	} else {
		s.log.Debugf("Sent Disable Data Streaming command")
	}
	time.Sleep(50 * time.Millisecond)

	// 2. Request BLE firmware version
	if err := writeUARTMessage(s.usock, ble.TypeBLEVersion, ble.TypeBLEVersionString, 0); err != nil {
		s.log.Warnf(" failed to request BLE firmware version: %v", err)
	} else {
		s.log.Debugf("Sent Request BLE Firmware Version command")
	}
	time.Sleep(50 * time.Millisecond)

	// 3. Request BLE MAC address
	if err := writeUARTMessage(s.usock, ble.TypeBLEParam, ble.TypeBLEParamMACAddress, 0); err != nil {
		s.log.Warnf(" failed to request BLE MAC address: %v", err)
	} else {
		s.log.Debugf("Sent Request BLE MAC Address command")
	}
	time.Sleep(50 * time.Millisecond)

	// 4. Enable data streaming
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 1); err != nil {
		s.log.Warnf(" failed to enable data streaming: %v", err)
	} else {
		s.log.Debugf("Sent Enable Data Streaming command")
	}
	time.Sleep(50 * time.Millisecond)

	// 5. Sync data stream
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamSync, 1); err != nil {
		s.log.Warnf(" Failed to sync data stream: %v", err)
	} else {
		s.log.Debugf("Sent Data Stream Sync command")
	}
	time.Sleep(50 * time.Millisecond)

	// 6. Start advertising (No Whitelist)
	if err := writeUARTMessage(s.usock, ble.TypeBLECommand, ble.SubType(ble.BLECommandAdvRestartNoWhitelist), 0); err != nil {
		s.log.Warnf(" failed to send command to restart advertising without whitelist: %v", err)
	} else {
		s.log.Debugf("Sent command to restart advertising without whitelist")
	}

	// Block until the version reply arrives or we time out. The version
	// handler does a non-blocking send on versionRxCh on receipt; a reply
	// that arrived during the steps above is sitting in the buffered
	// channel and is consumed immediately here.
	select {
	case <-s.versionRxCh:
		s.log.Infof("nRF52 initialization complete")
		return nil
	case <-time.After(nrfHandshakeTimeout):
		return fmt.Errorf("handshake timeout: no version response within %v", nrfHandshakeTimeout)
	case <-s.stopCh:
		return fmt.Errorf("handshake aborted: service stopping")
	}
}

// ShutdownNRF52 tells the nRF to stop its autonomous data stream before
// bluetooth-service exits. Without this, a restart of bluetooth-service
// leaves the stream running into nothing, and if the MDB suspends while
// bluetooth-service is down the UART frames will abort suspend.
func (s *Service) ShutdownNRF52() {
	if s.usock == nil {
		return
	}
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		s.log.Warnf("failed to disable data stream on shutdown: %v", err)
		return
	}
	s.log.Infof("Disabled nRF data stream on shutdown")
	// Give the nRF a moment to process before we close the serial port.
	time.Sleep(50 * time.Millisecond)
}

// RestartAdvertisingWithoutWhitelist sends command to restart advertising without whitelist
func (s *Service) RestartAdvertisingWithoutWhitelist() error {
	if err := writeUARTMessage(s.usock, ble.TypeBLECommand, ble.SubType(ble.BLECommandAdvRestartNoWhitelist), 0); err != nil {
		return fmt.Errorf("failed to send advertising restart command: %v", err)
	}
	s.log.Debugf("Sent command to restart advertising without whitelist")
	return nil
}

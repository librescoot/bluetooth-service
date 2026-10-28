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
	s.nrfInitMu.Lock()
	defer s.nrfInitMu.Unlock()
	s.invalidateNRFTimeRequest()
	s.nrfVersionMu.Lock()
	s.nrfVersionSeen = false
	s.nrfVersionMu.Unlock()
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

	// 2. Request the firmware version and negotiate optional host capabilities.
	// Firmware without HOST_SESSION support ignores the additional parameter and
	// still returns its version string.
	if err := s.negotiateHostSession(); err != nil {
		s.log.Warnf(" failed to request BLE firmware version: %v", err)
	} else {
		s.log.Debugf("Sent Request BLE Firmware Version command")
	}
	time.Sleep(50 * time.Millisecond)

	// 3. Publish the MDB version; the nRF does not re-request it after a restart.
	if err := s.pushMDBVersion(); err != nil {
		s.log.Warnf(" failed to send MDB firmware version: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// 4. Request BLE MAC address
	if err := writeUARTMessage(s.usock, ble.TypeBLEParam, ble.TypeBLEParamMACAddress, 0); err != nil {
		s.log.Warnf(" failed to request BLE MAC address: %v", err)
	} else {
		s.log.Debugf("Sent Request BLE MAC Address command")
	}
	time.Sleep(50 * time.Millisecond)

	// 5. Enable data streaming
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 1); err != nil {
		s.log.Warnf(" failed to enable data streaming: %v", err)
	} else {
		s.log.Debugf("Sent Enable Data Streaming command")
	}
	time.Sleep(50 * time.Millisecond)

	// 6. Sync data stream
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamSync, 1); err != nil {
		s.log.Warnf(" Failed to sync data stream: %v", err)
	} else {
		s.log.Debugf("Sent Data Stream Sync command")
	}
	time.Sleep(50 * time.Millisecond)

	// 7. Start advertising (No Whitelist)
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

// ShutdownNRF52 leaves the persistent nRF UART quiet at its 115200 boot baud
// before bluetooth-service exits. This allows either service implementation to
// initialize immediately after a process restart or MDB soft reboot.
func (s *Service) ShutdownNRF52() {
	// Block new reconnects before waiting for the current lifecycle owner.
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()

	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	if s.link != nil {
		// Stop keepalives and wait for any negotiation or fallback to release the
		// UART, but retain direct access for the shutdown commands below.
		s.link.Suspend()
		defer s.link.Stop()
	}

	if s.usock == nil {
		return
	}
	if err := writeUARTMessage(s.usock, ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		s.log.Warnf("failed to disable data stream on shutdown: %v", err)
	} else {
		s.log.Infof("Disabled nRF data stream on shutdown")
		// Keep BAUD_SET behind the stream-disable frame on the wire.
		time.Sleep(50 * time.Millisecond)
	}

	if s.link != nil {
		if err := s.link.EnsureLinkBaud(linkBaudDefault); err != nil {
			s.log.Warnf("failed to restore nRF boot baud on shutdown: %v", err)
		}
	}
}

// RestartAdvertisingWithoutWhitelist sends command to restart advertising without whitelist
func (s *Service) RestartAdvertisingWithoutWhitelist() error {
	if err := writeUARTMessage(s.usock, ble.TypeBLECommand, ble.SubType(ble.BLECommandAdvRestartNoWhitelist), 0); err != nil {
		return fmt.Errorf("failed to send advertising restart command: %v", err)
	}
	s.log.Debugf("Sent command to restart advertising without whitelist")
	return nil
}

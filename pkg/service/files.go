package service

import (
	"encoding/json"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/filetransfer"
	ipc "github.com/librescoot/redis-ipc"
)

const fileTunnelCapability = 0x04

func (s *Service) capabilityRegistry() string {
	registry := capabilityRegistryFor(s.nrfSupportsBondDelete(), s.tripCounterSupported())
	if s.files != nil && s.link != nil && s.link.Caps()&fileTunnelCapability != 0 {
		registry += ":files=1"
	}
	return registry
}

func (s *Service) EnableFileTransfer(logDir, inboxDir string) error {
	if err := s.ipc.Hash("power:inhibits").Delete("ble-files", ipc.Sync()); err != nil {
		return err
	}
	s.files = filetransfer.New(func() filetransfer.Writer { return s.GetUSock() }, map[byte]filetransfer.Store{
		filetransfer.StoreLogs:  {Dir: logDir, Suffix: ".tar.gz"},
		filetransfer.StoreInbox: {Dir: inboxDir, Writable: true},
	}, filetransfer.Hooks{Active: func(active bool) error {
		if !active {
			return s.ipc.Hash("power:inhibits").Delete("ble-files", ipc.Sync())
		}
		s.transferMu.Lock()
		defer s.transferMu.Unlock()
		if receiver, ok := s.ota.(interface{ Busy() bool }); ok && receiver.Busy() {
			return filetransfer.ErrTransportBusy
		}
		data, err := json.Marshal(map[string]any{"id": "ble-files", "who": "bluetooth-service", "what": "file-transfer", "why": "Bluetooth file transfer", "type": "block", "created": time.Now().Unix()})
		if err != nil {
			return err
		}
		return s.ipc.Hash("power:inhibits").Set("ble-files", string(data), ipc.Sync())
	}})
	return nil
}

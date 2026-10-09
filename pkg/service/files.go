package service

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/filetransfer"
	ipc "github.com/librescoot/redis-ipc"
)

const fileTunnelCapability = 0x04

func fileTransferCapabilities(tunnel, dataEnabled bool) string {
	if !tunnel {
		return ""
	}
	if dataEnabled {
		return ":files=1:data=1"
	}
	return ":files=1"
}

func (s *Service) capabilityRegistry() string {
	registry := capabilityRegistryFor(s.nrfSupportsBondDelete(), s.tripCounterSupported())
	if s.files != nil && s.link != nil {
		registry += fileTransferCapabilities(s.link.Caps()&fileTunnelCapability != 0, s.dataBrowserEnabled.Load())
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

// EnableDataBrowser enables administrative access rooted at /data.
func (s *Service) EnableDataBrowser() error {
	if s.files == nil {
		return errors.New("file transfer is not enabled")
	}
	if err := s.files.SetDataRoot("/data"); err != nil {
		return err
	}
	s.dataBrowserEnabled.Store(true)
	return nil
}

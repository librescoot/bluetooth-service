package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

type batteryIdentitySnapshot struct {
	Present      bool   `json:"present"`
	SerialNumber string `json:"serial_number,omitempty"`
}

func (s *Service) handleBatteryCommand(command string) {
	var slot string
	switch command {
	case "snapshot:0":
		slot = "0"
	case "snapshot:1":
		slot = "1"
	default:
		s.sendExtendedResponse("battery:error:unsupported")
		return
	}
	// One hash read keeps presence and identity from different pack updates
	// out of the same response. This is reported identity, not ownership proof.
	fields, err := s.ipc.HGetAll("battery:" + slot)
	if err != nil || (fields["present"] != "true" && fields["present"] != "false") {
		s.sendExtendedResponse("battery:error:unavailable")
		return
	}
	snapshot := batteryIdentitySnapshot{Present: fields["present"] == "true"}
	if snapshot.Present {
		snapshot.SerialNumber = canonicalBatterySerial(fields["serial-number"])
		if snapshot.SerialNumber == "" {
			s.sendExtendedResponse("battery:error:invalid-identity")
			return
		}
	}
	payload, _ := json.Marshal(snapshot)
	s.sendExtendedResponse(fmt.Sprintf("battery:snapshot:%s:%s", slot, payload))
}

func canonicalBatterySerial(serial string) string {
	serial = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, serial)
	serial = strings.Trim(serial, " ")
	if len(serial) == 0 || len(serial) > 64 {
		return ""
	}
	meaningful, hex := false, len(serial) == 16
	for _, r := range serial {
		if r < 0x21 || r > 0x7e {
			return ""
		}
		if (r >= '1' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			meaningful = true
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			hex = false
		}
	}
	switch strings.ToLower(serial) {
	case "unknown", "unbekannt", "n/a", "na", "none", "null", "nil", "undefined":
		return ""
	}
	if !meaningful {
		return ""
	}
	if hex {
		return strings.ToUpper(serial)
	}
	return serial
}

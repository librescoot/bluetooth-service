package service

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

// handleExtendedCommandMessage handles commands from the EXTENDED service (0x0400).
// Commands arrive as strings from the phone app via nRF, prefixed by topic.
func (s *Service) handleExtendedCommandMessage(msgType ble.MessageType, absSubTypeKey uint16, value interface{}) {
	cmdStr, ok := value.(string)
	if !ok {
		if cmdBytes, ok := value.([]byte); ok {
			cmdStr = string(cmdBytes)
		} else {
			s.log.Warnf("Extended command value is not a string: %T", value)
			return
		}
	}

	s.log.Infof("Received extended command: %s", cmdStr)

	if strings.HasPrefix(cmdStr, "nav:") {
		s.handleNavCommand(strings.TrimPrefix(cmdStr, "nav:"))
	} else if strings.HasPrefix(cmdStr, "keycard:") {
		s.handleKeycardCommand(strings.TrimPrefix(cmdStr, "keycard:"))
	} else if strings.HasPrefix(cmdStr, "usb:") {
		s.handleUSBCommand(strings.TrimPrefix(cmdStr, "usb:"))
	} else if strings.HasPrefix(cmdStr, "time:") {
		s.handleTimeCommand(strings.TrimPrefix(cmdStr, "time:"))
	} else if strings.HasPrefix(cmdStr, "config:") {
		s.handleConfigCommand(strings.TrimPrefix(cmdStr, "config:"))
	} else if strings.HasPrefix(cmdStr, "status:") {
		s.handleStatusQuery(strings.TrimPrefix(cmdStr, "status:"))
	} else if strings.HasPrefix(cmdStr, "cap:") {
		s.handleCapabilityQuery(strings.TrimPrefix(cmdStr, "cap:"))
	} else {
		s.log.Warnf("Unknown extended command prefix: %s", cmdStr)
		s.sendExtendedResponse("error:unknown command")
	}
}

// extRespMinInterval is the minimum spacing between consecutive extended
// responses. The nRF SoftDevice's BLE_GATTS_HVN_TX_QUEUE_SIZE_DEFAULT is 1,
// so back-to-back notifications get silently dropped (NRF_ERROR_RESOURCES,
// return value ignored in ble_scooter_param_write). 100 ms exceeds the
// configured MAX_CONN_INTERVAL of 75 ms, ensuring the previous notification
// has been transmitted before we queue the next one. This matters for
// multi-response commands like nav:fav:list, keycard:list, and cap:list.
const extRespMinInterval = 100 * time.Millisecond

// sendExtendedResponse sends a response string back to the nRF for the
// EXTENDED_RESPONSE characteristic. Calls are serialized and paced by
// extRespMinInterval to avoid overflowing the nRF notification queue.
func (s *Service) sendExtendedResponse(response string) {
	s.extRespMu.Lock()
	defer s.extRespMu.Unlock()

	if elapsed := time.Since(s.lastExtRespTime); elapsed < extRespMinInterval {
		time.Sleep(extRespMinInterval - elapsed)
	}

	if err := writeUARTMessageString(s.usock, ble.TypeExtended, ble.TypeExtendedResponse, response); err != nil {
		s.log.Errorf("Failed to send extended response: %v", err)
	}
	s.lastExtRespTime = time.Now()
}

// handleNavCommand processes navigation commands.
func (s *Service) handleNavCommand(cmd string) {
	if strings.HasPrefix(cmd, "dest ") {
		// "dest lat,lon" or "dest lat,lon,name"
		payload := strings.TrimPrefix(cmd, "dest ")
		parts := strings.SplitN(payload, ",", 3)
		if len(parts) < 2 {
			s.sendExtendedResponse("nav:error:invalid destination format")
			return
		}

		lat := strings.TrimSpace(parts[0])
		lon := strings.TrimSpace(parts[1])

		hash := s.ipc.Hash(KeyNavigation)
		if err := hash.Set("latitude", lat); err != nil {
			s.log.Errorf("Failed to set navigation latitude: %v", err)
			s.sendExtendedResponse("nav:error:redis")
			return
		}
		if err := hash.Set("longitude", lon); err != nil {
			s.log.Errorf("Failed to set navigation longitude: %v", err)
			s.sendExtendedResponse("nav:error:redis")
			return
		}
		// Legacy format
		coords := lat + "," + lon
		if err := hash.Set("destination", coords); err != nil {
			s.log.Errorf("Failed to set navigation destination: %v", err)
		}

		if len(parts) == 3 {
			name := strings.TrimSpace(parts[2])
			if err := hash.Set("address", name); err != nil {
				s.log.Errorf("Failed to set navigation address: %v", err)
			}
		}

		s.log.Infof("Set navigation destination: %s", payload)
		s.sendExtendedResponse("nav:ok")

	} else if cmd == "clear" {
		hash := s.ipc.Hash(KeyNavigation)
		for _, field := range []string{"latitude", "longitude", "destination", "address", "timestamp"} {
			if err := hash.Delete(field); err != nil {
				s.log.Errorf("Failed to clear navigation field %s: %v", field, err)
			}
		}
		s.log.Infof("Cleared navigation destination")
		s.sendExtendedResponse("nav:ok")

	} else if strings.HasPrefix(cmd, "fav:") {
		s.handleNavFavouriteCommand(strings.TrimPrefix(cmd, "fav:"))
	} else {
		s.sendExtendedResponse("nav:error:unknown command")
	}
}

// handleNavFavouriteCommand processes navigation favourite sub-commands.
func (s *Service) handleNavFavouriteCommand(cmd string) {
	if strings.HasPrefix(cmd, "add ") {
		// "add lat,lon,name"
		payload := strings.TrimPrefix(cmd, "add ")
		parts := strings.SplitN(payload, ",", 3)
		if len(parts) < 3 {
			s.sendExtendedResponse("nav:error:invalid favourite format (need lat,lon,name)")
			return
		}
		lat := strings.TrimSpace(parts[0])
		lon := strings.TrimSpace(parts[1])
		name := strings.TrimSpace(parts[2])

		id, err := s.addSavedLocation(lat, lon, name)
		if err != nil {
			s.log.Errorf("Failed to add saved location: %v", err)
			s.sendExtendedResponse(fmt.Sprintf("nav:error:%v", err))
			return
		}
		s.log.Infof("Added saved location %d: %s,%s %s", id, lat, lon, name)
		s.sendExtendedResponse(fmt.Sprintf("nav:fav:added:%d", id))

	} else if strings.HasPrefix(cmd, "delete ") {
		id := strings.TrimPrefix(cmd, "delete ")
		if err := s.deleteSavedLocation(id); err != nil {
			s.sendExtendedResponse(fmt.Sprintf("nav:error:%v", err))
			return
		}
		s.sendExtendedResponse("nav:ok")

	} else if strings.HasPrefix(cmd, "navigate ") {
		id := strings.TrimPrefix(cmd, "navigate ")
		if err := s.navigateToSavedLocation(id); err != nil {
			s.sendExtendedResponse(fmt.Sprintf("nav:error:%v", err))
			return
		}
		s.sendExtendedResponse("nav:ok")

	} else if cmd == "list" {
		s.listSavedLocations()
	} else {
		s.sendExtendedResponse("nav:error:unknown favourite command")
	}
}

// addSavedLocation adds a new saved location to Redis settings.
// Returns the assigned ID.
func (s *Service) addSavedLocation(lat, lon, name string) (int, error) {
	// Find next available ID by scanning existing entries
	settings := s.ipc.Hash("settings")
	id := 0
	for {
		existing, err := s.ipc.HGet("settings", fmt.Sprintf("dashboard.saved-locations.%d.latitude", id))
		if err != nil || existing == "" {
			break
		}
		id++
	}

	prefix := fmt.Sprintf("dashboard.saved-locations.%d", id)
	if err := settings.Set(prefix+".latitude", lat); err != nil {
		return 0, fmt.Errorf("failed to set latitude: %w", err)
	}
	if err := settings.Set(prefix+".longitude", lon); err != nil {
		return 0, fmt.Errorf("failed to set longitude: %w", err)
	}
	if err := settings.Set(prefix+".label", name); err != nil {
		return 0, fmt.Errorf("failed to set label: %w", err)
	}

	return id, nil
}

// deleteSavedLocation removes a saved location by ID.
func (s *Service) deleteSavedLocation(id string) error {
	settings := s.ipc.Hash("settings")
	prefix := fmt.Sprintf("dashboard.saved-locations.%s", id)
	for _, field := range []string{".latitude", ".longitude", ".label", ".created-at", ".last-used-at"} {
		if err := settings.Delete(prefix + field); err != nil {
			s.log.Warnf("Failed to delete %s%s: %v", prefix, field, err)
		}
	}
	s.log.Infof("Deleted saved location %s", id)
	return nil
}

// navigateToSavedLocation sets the navigation destination from a saved location.
func (s *Service) navigateToSavedLocation(id string) error {
	prefix := fmt.Sprintf("dashboard.saved-locations.%s", id)
	lat, err := s.ipc.HGet("settings", prefix+".latitude")
	if err != nil || lat == "" {
		return fmt.Errorf("saved location %s not found", id)
	}
	lon, err := s.ipc.HGet("settings", prefix+".longitude")
	if err != nil || lon == "" {
		return fmt.Errorf("saved location %s missing longitude", id)
	}

	hash := s.ipc.Hash(KeyNavigation)
	if err := hash.Set("latitude", lat); err != nil {
		return fmt.Errorf("failed to set latitude: %w", err)
	}
	if err := hash.Set("longitude", lon); err != nil {
		return fmt.Errorf("failed to set longitude: %w", err)
	}
	if err := hash.Set("destination", lat+","+lon); err != nil {
		return fmt.Errorf("failed to set destination: %w", err)
	}

	s.log.Infof("Navigating to saved location %s: %s,%s", id, lat, lon)
	return nil
}

// listSavedLocations sends all saved locations as a sequence of responses.
func (s *Service) listSavedLocations() {
	count := 0
	var entries []string

	for id := 0; ; id++ {
		prefix := fmt.Sprintf("dashboard.saved-locations.%d", id)
		lat, err := s.ipc.HGet("settings", prefix+".latitude")
		if err != nil || lat == "" {
			// Check a few more in case of gaps
			if id < 100 {
				continue
			}
			break
		}
		lon, _ := s.ipc.HGet("settings", prefix+".longitude")
		label, _ := s.ipc.HGet("settings", prefix+".label")
		entries = append(entries, fmt.Sprintf("nav:fav:%d:%s,%s,%s", id, lat, lon, label))
		count++
	}

	s.sendExtendedResponse(fmt.Sprintf("nav:fav:count:%d", count))
	for _, entry := range entries {
		s.sendExtendedResponse(entry)
	}
}

// handleUSBCommand processes USB mode commands.
func (s *Service) handleUSBCommand(cmd string) {
	switch cmd {
	case "ums":
		if err := s.ipc.Hash(KeyUSB).Set("mode", "ums"); err != nil {
			s.log.Errorf("Failed to set USB mode to UMS: %v", err)
			s.sendExtendedResponse("usb:error:redis")
			return
		}
		s.log.Infof("Set USB mode: ums")
		s.sendExtendedResponse("usb:ok")
	case "normal":
		if err := s.ipc.Hash(KeyUSB).Set("mode", "normal"); err != nil {
			s.log.Errorf("Failed to set USB mode to normal: %v", err)
			s.sendExtendedResponse("usb:error:redis")
			return
		}
		s.log.Infof("Set USB mode: normal")
		s.sendExtendedResponse("usb:ok")
	default:
		s.sendExtendedResponse("usb:error:unknown mode")
	}
}

// handleKeycardCommand processes keycard management commands.
// Commands are forwarded to the scooter:keycard Redis list for
// processing by keycard-service.
func (s *Service) handleKeycardCommand(cmd string) {
	s.log.Infof("Received keycard command: %s", cmd)

	switch {
	case cmd == "list", cmd == "count",
		strings.HasPrefix(cmd, "add:"),
		strings.HasPrefix(cmd, "remove:"):
		if err := ipc.SendRequest(s.ipc, "scooter:keycard", cmd); err != nil {
			s.log.Errorf("Failed to forward keycard command: %v", err)
			s.sendExtendedResponse("keycard:error:redis")
			return
		}
		// Response comes asynchronously via keycard hash subscription
	default:
		s.sendExtendedResponse("keycard:error:unknown command")
	}
}

// handleTimeCommand processes time-setting commands.
func (s *Service) handleTimeCommand(cmd string) {
	if strings.HasPrefix(cmd, "set ") {
		timestamp := strings.TrimSpace(strings.TrimPrefix(cmd, "set "))
		if err := s.setSystemTime(timestamp); err != nil {
			s.sendExtendedResponse(fmt.Sprintf("time:error:%v", err))
			return
		}
		s.sendExtendedResponse("time:ok")
	} else {
		s.sendExtendedResponse("time:error:unknown command")
	}
}

// setSystemTime parses a Unix timestamp string and sets the system clock
// via timedatectl.
func (s *Service) setSystemTime(timestampStr string) error {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampStr), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	t := time.Unix(timestamp, 0).UTC()
	timeStr := t.Format("2006-01-02 15:04:05")
	cmd := exec.Command("timedatectl", "set-time", timeStr)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to set time: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	s.log.Infof("System time set to %s", t.Format(time.RFC3339))
	return nil
}

// handleConfigCommand processes configuration commands.
// Supported: config:apn <value>, config:hibernate-timer <seconds>,
// config:update-channel <channel>
func (s *Service) handleConfigCommand(cmd string) {
	parts := strings.SplitN(cmd, " ", 2)
	if len(parts) < 2 {
		s.sendExtendedResponse("config:error:missing value")
		return
	}
	key := parts[0]
	value := strings.TrimSpace(parts[1])

	settings := s.ipc.Hash("settings")

	switch key {
	case "apn":
		if err := settings.Set("cellular.apn", value); err != nil {
			s.log.Errorf("Failed to set cellular APN: %v", err)
			s.sendExtendedResponse("config:error:redis")
			return
		}
		s.log.Infof("Set cellular APN: %s", value)
		s.sendExtendedResponse("config:ok")

	case "hibernate-timer":
		if _, err := strconv.Atoi(value); err != nil {
			s.sendExtendedResponse("config:error:invalid number")
			return
		}
		if err := settings.Set("hibernation-timer", value); err != nil {
			s.log.Errorf("Failed to set hibernation timer: %v", err)
			s.sendExtendedResponse("config:error:redis")
			return
		}
		s.log.Infof("Set hibernation timer: %s seconds", value)
		s.sendExtendedResponse("config:ok")

	case "auto-standby-seconds":
		seconds, err := strconv.Atoi(value)
		if err != nil {
			s.sendExtendedResponse("config:error:invalid number")
			return
		}
		if seconds < 0 || seconds > 3600 {
			s.sendExtendedResponse("config:error:out of range (0-3600)")
			return
		}
		if err := settings.Set("scooter.auto-standby-seconds", value); err != nil {
			s.log.Errorf("Failed to set auto-standby-seconds: %v", err)
			s.sendExtendedResponse("config:error:redis")
			return
		}
		s.log.Infof("Set auto-standby-seconds: %d seconds", seconds)
		s.sendExtendedResponse("config:ok")

	case "update-channel":
		switch value {
		case "stable", "testing", "nightly":
		default:
			s.sendExtendedResponse("config:error:invalid channel (stable/testing/nightly)")
			return
		}
		if err := settings.Set("updates.mdb.channel", value); err != nil {
			s.log.Errorf("Failed to set MDB update channel: %v", err)
			s.sendExtendedResponse("config:error:redis")
			return
		}
		if err := settings.Set("updates.dbc.channel", value); err != nil {
			s.log.Errorf("Failed to set DBC update channel: %v", err)
			s.sendExtendedResponse("config:error:redis")
			return
		}
		s.log.Infof("Set update channel: %s", value)
		s.sendExtendedResponse("config:ok")

	default:
		s.sendExtendedResponse("config:error:unknown setting")
	}
}

// handleStatusQuery processes read-only status queries.
func (s *Service) handleStatusQuery(key string) {
	key = strings.TrimSpace(key)

	switch key {
	case "maps-available":
		val, err := s.ipc.HGet("dashboard", "maps-available")
		if err != nil || val == "" {
			val = "false"
		}
		s.sendExtendedResponse(fmt.Sprintf("status:maps-available:%s", val))

	case "navigation-available":
		val, err := s.ipc.HGet("dashboard", "navigation-available")
		if err != nil || val == "" {
			val = "false"
		}
		s.sendExtendedResponse(fmt.Sprintf("status:navigation-available:%s", val))

	default:
		s.sendExtendedResponse("status:error:unknown key")
	}
}

// capabilityMap maps each command category to its supported commands.
var capabilityMap = map[string][]string{
	"nav":     {"dest", "clear", "fav:add", "fav:delete", "fav:navigate", "fav:list"},
	"keycard": {"list", "count", "add", "remove"},
	"usb":     {"ums", "normal"},
	"time":    {"set"},
	"config":  {"apn", "hibernate-timer", "update-channel", "auto-standby-seconds"},
	"status":  {"maps-available", "navigation-available"},
	"cap":     {"list"},
}

// handleCapabilityQuery responds with the list of supported extended command features.
// "cap:list" returns all category names.
// "cap:<category>" returns the commands supported by that category.
func (s *Service) handleCapabilityQuery(cmd string) {
	cmd = strings.TrimSpace(cmd)

	if cmd == "list" {
		categories := make([]string, 0, len(capabilityMap))
		for cat := range capabilityMap {
			categories = append(categories, cat)
		}
		s.sendExtendedResponse(fmt.Sprintf("cap:count:%d", len(categories)))
		for _, cat := range categories {
			s.sendExtendedResponse(fmt.Sprintf("cap:%s", cat))
		}
		return
	}

	if commands, ok := capabilityMap[cmd]; ok {
		s.sendExtendedResponse(fmt.Sprintf("cap:%s:count:%d", cmd, len(commands)))
		for _, c := range commands {
			s.sendExtendedResponse(fmt.Sprintf("cap:%s:%s", cmd, c))
		}
		return
	}

	s.sendExtendedResponse("cap:error:unknown command")
}

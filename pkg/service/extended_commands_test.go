package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// timedatectl set-time interprets its argument in the system-local timezone.
// systemTimeArg must therefore format in local time so the arg round-trips
// back to the intended instant. The old UTC formatting set the clock off by
// the local UTC offset whenever local != UTC.
func TestSystemTimeArgUsesLocalZone(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	orig := time.Local
	time.Local = berlin
	defer func() { time.Local = orig }()

	// 2024-07-01 12:00:00 UTC == 14:00 CEST (+02:00).
	const epoch int64 = 1719835200

	arg := systemTimeArg(epoch)
	if arg != "2024-07-01 14:00:00" {
		t.Errorf("systemTimeArg = %q, want local-zone %q", arg, "2024-07-01 14:00:00")
	}

	// Mimic timedatectl: parse the arg back in the local zone; it must equal
	// the original instant.
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", arg, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(time.Unix(epoch, 0)) {
		t.Errorf("round-trip = %v, want %v", parsed.UTC(), time.Unix(epoch, 0).UTC())
	}
}

func TestTimeSetUsesInjectedClockAndSeedsNRF(t *testing.T) {
	mock := &mockUSOCK{}
	calls := 0
	s := &Service{
		usock:       mock,
		log:         logger.NewLogger(nil, logger.LogLevelNone),
		nrfTimeNow:  func() time.Time { return time.Unix(testNow, 0) },
		clockSetter: func(epoch int64) error { calls++; return nil },
	}
	s.handleTimeCommand("set 1735689600")
	if calls != 1 {
		t.Fatalf("clock setter calls = %d, want 1", calls)
	}
	if mock.messageCount() < 1 || mock.messages[0].frameID != byte(uint16(ble.TypeRTC)&0xff) {
		t.Fatalf("trusted time command did not seed nRF RTC: %#v", mock.messages)
	}
}

func TestLegacyBLETimeSetsAndSeedsNRF(t *testing.T) {
	mock := &mockUSOCK{}
	calls := 0
	s := &Service{
		usock:       mock,
		log:         logger.NewLogger(nil, logger.LogLevelNone),
		nrfTimeNow:  func() time.Time { return time.Unix(testNow, 0) },
		clockSetter: func(epoch int64) error { calls++; return nil },
	}
	s.handleScooterInfoMessage(
		ble.TypeScooterInfo,
		uint16(ble.TypeScooterInfo)+uint16(ble.TypeSystemTime),
		"1735689600",
	)
	if calls != 1 {
		t.Fatalf("clock setter calls = %d, want 1", calls)
	}
	if mock.messageCount() != 1 || mock.messages[0].frameID != byte(uint16(ble.TypeRTC)&0xff) {
		t.Fatalf("BLE time did not seed nRF RTC: %#v", mock.messages)
	}
}

func TestTimeSetFailureDoesNotSeedNRF(t *testing.T) {
	mock := &mockUSOCK{}
	s := &Service{
		usock:       mock,
		log:         logger.NewLogger(nil, logger.LogLevelNone),
		nrfTimeNow:  func() time.Time { return time.Unix(testNow, 0) },
		clockSetter: func(int64) error { return errors.New("denied") },
	}
	s.handleTimeCommand("set 1735689600")
	for _, message := range mock.messages {
		if message.frameID == byte(uint16(ble.TypeRTC)&0xff) {
			t.Fatal("failed explicit time set seeded nRF RTC")
		}
	}
}

func TestNrfBondDeleteSupported(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"v2.8.0-ls", true},
		{"2.8.0", true},
		{"v2.8.1-ls", true},
		{"v2.9.0-ls", true},
		{"v3.0.0-ls", true},
		{"v2.8.0-3-gabc123-ls", true},
		{"v2.7.2-ls", false},
		{"v2.7.2-5-gdeadbee-ls", false},
		{"v1.12.0", false},
		{"", false},
		{"not-a-version", false},
	}

	for _, tt := range tests {
		if got := nrfBondDeleteSupported(tt.version); got != tt.want {
			t.Errorf("nrfBondDeleteSupported(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}

func TestDBCWaitReached(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		power       string
		ready       string
		wantReached bool
	}{
		{"on and ready", "on-wait", "on", "true", true},
		{"on but not ready", "on-wait", "on", "false", false},
		{"stale ready while off", "on-wait", "off", "true", false},
		{"off", "off-wait", "off", "false", true},
		{"off ignores stale ready", "off-wait", "off", "true", true},
		{"still on", "off-wait", "on", "false", false},
		{"unknown command", "wait", "on", "true", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dbcWaitReached(tt.command, tt.power, tt.ready); got != tt.wantReached {
				t.Errorf("dbcWaitReached(%q, %q, %q) = %v, want %v", tt.command, tt.power, tt.ready, got, tt.wantReached)
			}
		})
	}
}

// cap:ble is what the app probes before offering to clear the scooter side of a
// bond, so it has to track the nRF rather than what this binary was built with.
func TestLegacyCapabilityMapDoesNotAdvertiseCapExtOnlyGroups(t *testing.T) {
	for _, category := range []string{"nav", "keycard", "usb", "time", "config", "status", "alarm", "ltc", "ble", "pm", "dbc", "ota", "cap", "get", "set"} {
		if _, ok := capabilityMap[category]; !ok {
			t.Errorf("legacy capability category %q disappeared", category)
		}
	}
	for _, category := range []string{"settings", "trip"} {
		if _, ok := capabilityMap[category]; ok {
			t.Errorf("cap:%s unexpectedly changed the deployed cap:list contract", category)
		}
	}
}

func TestTripCapabilityPromotesCurrentSchemaForGenericSettings(t *testing.T) {
	staleSchema := map[string]settingSchema{
		"legacy.setting": {Type: "bool"},
	}
	currentSchema := map[string]settingSchema{
		"trip.counter-reset": {
			Type: "enum",
			Values: []settingSchemaValue{
				{Value: "ride"}, {Value: "day"}, {Value: "battery"}, {Value: "manual"},
			},
		},
	}
	s := &Service{schemaCache: staleSchema}
	cached, err := s.getSettingsSchema()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cached["trip.counter-reset"]; ok {
		t.Fatal("test setup unexpectedly includes trip.counter-reset in stale schema")
	}

	// Capability discovery fetches the current schema independently of the
	// generic command cache after settings-service has restarted.
	tripSupported := tripCounterCapabilitySupported("1", "1", currentSchema)
	if !tripSupported {
		t.Fatal("current schema did not enable the trip capability")
	}
	s.promoteSettingsSchema(currentSchema)
	if registry := capabilityRegistryFor(false, tripSupported); !strings.HasSuffix(registry, ":trip") {
		t.Fatalf("cap:ext registry = %q, want trip capability", registry)
	}

	// Generic get and set both source their key and validation data from this
	// shared cache, so they must see the schema that capability discovery used.
	schema, err := s.getSettingsSchema()
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := schema["trip.counter-reset"]
	if !ok {
		t.Fatal("generic get would reject trip.counter-reset as an unknown key")
	}
	if got := validateSettingValue(spec, "manual"); got != "" {
		t.Fatalf("generic set validation = %q, want valid", got)
	}
}

func TestGenericSetValidationSupportsTripExpungeFormat(t *testing.T) {
	key, value, ok := splitSetPayload("trip.expunge:age:365d")
	if !ok || key != "trip.expunge" || value != "age:365d" {
		t.Fatalf("split generic set = (%q, %q, %v)", key, value, ok)
	}

	var schema map[string]settingSchema
	if err := json.Unmarshal([]byte(`{"trip.expunge":{"type":"string","format":"trip-expunge"}}`), &schema); err != nil {
		t.Fatal(err)
	}
	spec := schema["trip.expunge"]
	if spec.Format != "trip-expunge" {
		t.Fatalf("format = %q, want trip-expunge", spec.Format)
	}

	for _, value := range []string{
		"never",
		"age:1ns",
		"age:1us",
		"age:1.5ms",
		"age:1h30m",
		"age:1d",
		"age:106751d",
		"count:0",
		"count:9223372036854775807",
		"size:0",
		"size:9223372036854775807",
	} {
		if got := validateSettingValue(spec, value); got != "" {
			t.Errorf("validateSettingValue(%q) = %q, want valid", value, got)
		}
	}

	for _, value := range []string{
		"age:0",
		"age:0ns",
		"age:0.5ns",
		"age:.5us",
		"age:1.s",
		"age:1µs",
		"age:1μs",
		"age:106752d",
		"age:2562047h47m16.854775808s",
		"count:01",
		"count:+1",
		"count:9223372036854775808",
		"size:-1",
		"size:9223372036854775808",
		"never ",
		"age: 1h",
		"age:1h ",
		"age:1h\u00a0",
		"count: 1",
		"size:1 ",
		"",
		"age:0s",
		"age:0d",
		"age:+1h",
		"age:-1h",
		"age:01d",
		"age:01ns",
		"age:1d1h",
		"age:18446744073709551616d",
		"age:1D",
		"age:1day",
		"age:1htrailing",
		"age:999999999999999999999h",
		"count:-1",
		"count:18446744073709551616",
		"size:1.0",
		"size:01",
		"size:18446744073709551616",
		"unknown:1",
	} {
		if got := validateSettingValue(spec, value); got != "invalid trip expunge" {
			t.Errorf("validateSettingValue(%q) = %q, want invalid trip expunge", value, got)
		}
	}

	if got := validateSettingValue(settingSchema{Type: "future-type"}, "future-value"); got != "" {
		t.Fatalf("unknown type lost compatibility: %q", got)
	}
}

func TestCapabilityCommandsForBLETracksFirmware(t *testing.T) {
	supported := capabilityCommandsFor("ble", func() bool { return true })
	if len(supported) != 1 || supported[0] != "forget" {
		t.Errorf("ble capabilities on new firmware = %v, want [forget]", supported)
	}

	unsupported := capabilityCommandsFor("ble", func() bool { return false })
	if len(unsupported) != 0 {
		t.Errorf("ble capabilities on old firmware = %v, want none", unsupported)
	}

	// Other categories are fixed at build time and must not consult the nRF.
	nav := capabilityCommandsFor("nav", func() bool {
		t.Error("nav capabilities consulted the nRF firmware version")
		return false
	})
	if len(nav) != len(capabilityMap["nav"]) {
		t.Errorf("nav capabilities = %v, want %v", nav, capabilityMap["nav"])
	}

	dbc := capabilityCommandsFor("dbc", func() bool {
		t.Error("dbc capabilities consulted the nRF firmware version")
		return false
	})
	if len(dbc) != 5 {
		t.Errorf("dbc capabilities = %v, want five commands", dbc)
	}
}

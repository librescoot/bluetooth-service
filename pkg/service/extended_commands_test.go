package service

import (
	"testing"
	"time"
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

// The single-bond delete was a silent no-op in the nRF until v2.8.0-ls, so the
// gate has to answer no for anything older and for a version it cannot read.
// Answering yes there would have the phone report both halves of the bond
// cleared while the scooter kept its half.
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

// cap:ble is what the app probes before offering to clear the scooter side of a
// bond, so it has to track the nRF rather than what this binary was built with.
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
}

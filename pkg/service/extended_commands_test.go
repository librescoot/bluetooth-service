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

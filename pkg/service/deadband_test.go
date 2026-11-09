package service

import (
	"testing"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
)

func TestDeadbandFilter(t *testing.T) {
	rule := deadbandRule{band: 10, maxAge: 30 * time.Second}
	var f deadbandFilter
	t0 := time.Unix(1_700_000_000, 0)
	steps := []struct {
		name  string
		value int64
		at    time.Duration
		want  bool
	}{
		{"first value", 100, 0, true},
		{"inside band", 105, time.Second, false},
		{"inside band, other side", 95, 2 * time.Second, false},
		{"exactly the band", 110, 3 * time.Second, true},
		{"measured against the last write, not the last report", 115, 4 * time.Second, false},
		{"outside band", 90, 5 * time.Second, true},
		{"unchanged but not stale", 90, 34 * time.Second, false},
		{"unchanged and stale", 90, 36 * time.Second, true},
	}
	for _, s := range steps {
		if got := f.admit("k", s.value, rule, t0.Add(s.at)); got != s.want {
			t.Errorf("%s: admit(%d) = %t, want %t", s.name, s.value, got, s.want)
		}
	}
	if !f.admit("other", 90, rule, t0.Add(36*time.Second)) {
		t.Error("keys must be independent")
	}
	f.forget("k")
	if !f.admit("k", 90, rule, t0.Add(37*time.Second)) {
		t.Error("a forgotten key must be written again")
	}
}

func TestCBBatteryNoiseIsNotWrittenToRedis(t *testing.T) {
	s, mr := newVersionPushService(t)

	push := func(sub ble.SubType, v int64) { s.handleBatteryInfoMessage(sub, v) }
	read := func(field string) string { return mr.HGet("cb-battery", field) }
	waitFor := func(field, want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for read(field) != want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := read(field); got != want {
			t.Fatalf("%s = %q, want %q", field, got, want)
		}
	}

	push(ble.TypeBatteryInfoCellVoltage, 4_172_109)
	push(ble.TypeBatteryInfoCurrent, -33_854)
	waitFor("cell-voltage", "4172109")
	waitFor("current", "-33854")

	// Sensor noise stays out; the published value remains the first one.
	push(ble.TypeBatteryInfoCellVoltage, 4_172_500)
	push(ble.TypeBatteryInfoCellVoltage, 4_173_593)
	push(ble.TypeBatteryInfoCurrent, -36_458)
	time.Sleep(100 * time.Millisecond)
	if got := read("cell-voltage"); got != "4172109" {
		t.Errorf("cell-voltage noise was written: %q", got)
	}
	if got := read("current"); got != "-33854" {
		t.Errorf("current noise was written: %q", got)
	}

	// A real change goes through at once, and unbanded fields are untouched.
	push(ble.TypeBatteryInfoCellVoltage, 4_150_000)
	push(ble.TypeBatteryInfoCurrent, 450_000)
	push(ble.TypeBatteryInfoCharge, 98)
	waitFor("cell-voltage", "4150000")
	waitFor("current", "450000")
	waitFor("charge", "98")
}

func TestOutsideTemperatureDeadband(t *testing.T) {
	s, mr := newVersionPushService(t)
	push := func(tenths int64) { s.handleVehicleStateMessage(ble.TypeExternalTemperature, tenths) }
	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for mr.HGet("scooter", "temperature") != want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := mr.HGet("scooter", "temperature"); got != want {
			t.Fatalf("temperature = %q, want %q", got, want)
		}
	}
	push(51)
	waitFor("5.1")
	push(50)
	push(52)
	time.Sleep(100 * time.Millisecond)
	if got := mr.HGet("scooter", "temperature"); got != "5.1" {
		t.Errorf("0.1 °C jitter was written: %q", got)
	}
	push(43)
	waitFor("4.3")
}

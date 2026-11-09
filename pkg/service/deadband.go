package service

import (
	"sync"
	"time"
)

// deadbandRule suppresses a numeric write while the value stays within band of
// the last written one, until maxAge forces a refresh. The nRF reports the
// backup cell and the outside temperature about once a second, and ADC noise
// moves those readings on nearly every report. Each write is an HSET plus a
// PUBLISH that three or four other services react to, so on a parked scooter
// they were the bulk of the redis traffic.
type deadbandRule struct {
	band   int64
	maxAge time.Duration
}

// Raw units as the nRF sends them: micro-volts and micro-amperes for the backup
// cell, tenths of a degree for the outside temperature.
var (
	cbBatteryDeadband = map[string]deadbandRule{
		"cell-voltage": {band: 10_000, maxAge: 30 * time.Second}, // 10 mV
		"current":      {band: 20_000, maxAge: 30 * time.Second}, // 20 mA
	}
	scooterTemperatureDeadband = deadbandRule{band: 2, maxAge: 60 * time.Second} // 0.2 °C
)

type deadbandEntry struct {
	value int64
	at    time.Time
}

type deadbandFilter struct {
	mu   sync.Mutex
	last map[string]deadbandEntry
}

// admit reports whether value must be written now and records it if so.
func (f *deadbandFilter) admit(key string, value int64, rule deadbandRule, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last == nil {
		f.last = make(map[string]deadbandEntry)
	}
	if e, ok := f.last[key]; ok && now.Sub(e.at) < rule.maxAge && abs64(value-e.value) < rule.band {
		return false
	}
	f.last[key] = deadbandEntry{value: value, at: now}
	return true
}

// forget drops a key so the next value is written, after a failed write.
func (f *deadbandFilter) forget(key string) {
	f.mu.Lock()
	delete(f.last, key)
	f.mu.Unlock()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

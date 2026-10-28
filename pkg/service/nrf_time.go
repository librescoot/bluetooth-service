package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
)

const (
	nrfTimeResponseWindow = 2 * time.Second
	nrfTimePlausibility   = int64(1704067200) // 2024-01-01T00:00:00Z
)

// nrfTimeMaximumHoldoverYears accommodates long parked intervals while
// bounding corruption without deriving elapsed outage from oscillator drift.
// AddDate preserves calendar boundaries.
const nrfTimeMaximumHoldoverYears = 5

type nrfTimeState struct {
	pending    bool
	generation uint64
	token      uint64
}

func (s *Service) timeNow() time.Time {
	if s.nrfTimeNow != nil {
		return s.nrfTimeNow()
	}
	return time.Now()
}

func (s *Service) nrfTimePlausible(epoch int64, now time.Time) bool {
	if epoch < nrfTimePlausibility {
		return false
	}
	return !time.Unix(epoch, 0).After(now.AddDate(nrfTimeMaximumHoldoverYears, 0, 0))
}

func writeUARTMessageRTCGet(sock usockWriter, token uint64) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}
	message := map[uint16]map[uint16]interface{}{
		uint16(ble.TypeRTC): {
			uint16(ble.TypeRTC) + uint16(ble.TypeRTCGet):   nil,
			uint16(ble.TypeRTC) + uint16(ble.TypeRTCToken): token,
		},
	}
	data, err := cbor.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal CBOR: %w", err)
	}
	return sock.WriteWithFrameID(byte(uint16(ble.TypeRTC)&0xff), data)
}

func writeUARTMessage64(sock usockWriter, messageType ble.MessageType, subType ble.SubType, value uint64) error {
	if sock == nil {
		return fmt.Errorf("USOCK connection is not initialized")
	}
	absoluteKey := uint16(messageType) + uint16(subType)
	message := map[uint16]map[uint16]uint64{
		uint16(messageType): {absoluteKey: value},
	}
	data, err := cbor.Marshal(message)
	if err != nil {
		return fmt.Errorf("failed to marshal CBOR: %w", err)
	}
	return sock.WriteWithFrameID(byte(messageType), data)
}

func newNRFTimeToken() (uint64, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return 0, err
	}
	token := binary.LittleEndian.Uint64(data[:])
	if token == 0 {
		token = 1
	}
	return token, nil
}

// invalidateNRFTimeRequest prevents replies from a previous UART session from
// being accepted while initialization is in progress.
func (s *Service) invalidateNRFTimeRequest() {
	s.nrfTimeMu.Lock()
	s.nrfTimeState.generation++
	s.nrfTimeState.pending = false
	s.nrfTimeState.token = 0
	s.nrfTimeMu.Unlock()
}

// requestNRFTime probes the USOCK-only RTC without delaying initialization.
func (s *Service) requestNRFTime() {
	token, err := newNRFTimeToken()
	if err != nil {
		s.log.Warnf("nRF RTC unavailable: could not create request token: %v", err)
		return
	}

	s.nrfTimeMu.Lock()
	s.nrfTimeState.generation++
	generation := s.nrfTimeState.generation
	s.nrfTimeState.pending = true
	s.nrfTimeState.token = token
	s.nrfTimeMu.Unlock()

	if err := writeUARTMessageRTCGet(s.usock, token); err != nil {
		s.nrfTimeMu.Lock()
		if s.nrfTimeState.generation == generation {
			s.nrfTimeState.pending = false
			s.nrfTimeState.token = 0
		}
		s.nrfTimeMu.Unlock()
		s.log.Warnf("nRF RTC unavailable: request failed: %v", err)
		return
	}
	s.log.Infof("Requested nRF RTC restoration sample")
	go func() {
		time.Sleep(nrfTimeResponseWindow)
		s.expireNRFTimeRequest(generation)
	}()
}

func (s *Service) expireNRFTimeRequest(generation uint64) {
	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	if !s.nrfTimeState.pending || s.nrfTimeState.generation != generation {
		return
	}
	s.nrfTimeState.pending = false
	s.nrfTimeState.token = 0
	s.log.Infof("nRF RTC unavailable: no response (legacy firmware or invalid clock)")
}

func (s *Service) handleNRFTimeReply(params map[uint16]interface{}) {
	getKey := uint16(ble.TypeRTC) + uint16(ble.TypeRTCGet)
	tokenKey := uint16(ble.TypeRTC) + uint16(ble.TypeRTCToken)
	value, hasValue := params[getKey]
	token, hasToken := params[tokenKey].(uint64)
	if len(params) != 2 || !hasValue || !hasToken {
		s.log.Debugf("Ignored malformed nRF RTC reply")
		return
	}

	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	if !s.nrfTimeState.pending || s.nrfTimeState.token != token {
		s.log.Debugf("Ignored unsolicited, duplicate, or stale nRF RTC reply")
		return
	}
	s.nrfTimeState.pending = false
	s.nrfTimeState.token = 0

	epoch, ok := nrfTimeValue(value)
	if !ok {
		s.log.Warnf("nRF RTC sample rejected: invalid state")
		return
	}

	now := s.timeNow()
	if !s.nrfTimePlausible(epoch, now) {
		s.log.Warnf("nRF RTC sample rejected: implausible epoch %d", epoch)
		return
	}
	if epoch <= now.Unix() {
		s.log.Infof("nRF RTC sample not applied: Linux clock is already current")
		return
	}
	current, applied, err := s.setClockIfNewer(epoch)
	if err != nil {
		s.log.Errorf("nRF RTC restoration failed: %v", err)
		return
	}
	if !applied {
		s.log.Infof("nRF RTC sample not applied: Linux clock is already current")
		return
	}
	s.log.Infof("Restored Linux clock from nRF RTC: %d -> %d", current, epoch)
}

func nrfTimeValue(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v), true
		}
	case int64:
		if v >= 0 {
			return v, true
		}
	}
	return 0, false
}

// seedNRFTime copies a trusted Linux clock to the retained RTC.
func (s *Service) seedNRFTime(epoch int64, source string) bool {
	if !s.nrfTimePlausible(epoch, s.timeNow()) {
		s.log.Warnf("nRF RTC seed rejected: Linux clock is implausible")
		return false
	}
	if err := writeUARTMessage64(s.usock, ble.TypeRTC, ble.TypeRTCSet, uint64(epoch)); err != nil {
		s.log.Warnf("nRF RTC seed failed: %v", err)
		return false
	}
	s.log.Infof("Seeded nRF RTC from %s: %d", source, epoch)
	return true
}

const trustedTimePollInterval = 5 * time.Minute

// StartTrustedTimeMonitor seeds the nRF only after a trusted source validates
// the Linux clock. It is deliberately separate from startup restoration.
func (s *Service) StartTrustedTimeMonitor() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-s.stopCh:
			return
		case <-timer.C:
		}
		ticker := time.NewTicker(trustedTimePollInterval)
		defer ticker.Stop()
		for {
			s.syncTrustedTime()
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

func (s *Service) syncTrustedTime() {
	gpsSync := s.gpsClockSync()
	ntpTrusted := s.ntpTrusted()

	s.trustedTimeMu.Lock()
	defer s.trustedTimeMu.Unlock()
	if gpsSync != "" && gpsSync != s.lastGPSSync {
		if s.seedNRFTime(s.timeNow().Unix(), "GPS-confirmed Linux clock") {
			s.lastGPSSync = gpsSync
		}
		s.lastNTPTrusted = ntpTrusted
		return
	}
	if ntpTrusted && !s.lastNTPTrusted {
		s.lastNTPTrusted = s.seedNRFTime(s.timeNow().Unix(), "NTP-confirmed Linux clock")
		return
	}
	s.lastNTPTrusted = ntpTrusted
}

func (s *Service) gpsClockSync() string {
	if s.trustedTimeGPS != nil {
		return s.trustedTimeGPS()
	}
	if s.ipc == nil {
		return ""
	}
	source, err := s.ipc.HGet("clock", "source")
	if err != nil || source != "gps" {
		return ""
	}
	syncedAt, err := s.ipc.HGet("clock", "synced-at")
	if err != nil {
		return ""
	}
	return syncedAt
}

func (s *Service) ntpTrusted() bool {
	if s.trustedTimeNTP != nil {
		return s.trustedTimeNTP()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "chronyc", "tracking").Output()
	if err != nil {
		return false
	}
	refID, leap := parseNTPTracking(string(out))
	return leap == "Normal" && !pseudoNTPReference(refID)
}

func parseNTPTracking(out string) (refID, leap string) {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Reference ID":
			if fields := strings.Fields(strings.TrimSpace(value)); len(fields) > 0 {
				refID = fields[0]
			}
		case "Leap status":
			leap = strings.TrimSpace(value)
		}
	}
	return refID, leap
}

func pseudoNTPReference(refID string) bool {
	refID = strings.TrimSpace(refID)
	if refID == "" {
		return true
	}
	if strings.Contains(refID, ".") {
		if ip := net.ParseIP(refID); ip != nil {
			return ip.IsLoopback()
		}
	}
	return strings.HasPrefix(refID, "7F")
}

func (s *Service) setClock(epoch int64) error {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	return s.setClockLocked(epoch)
}

func (s *Service) setClockIfNewer(epoch int64) (current int64, applied bool, err error) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	current = s.timeNow().Unix()
	if epoch <= current {
		return current, false, nil
	}
	if err := s.setClockLocked(epoch); err != nil {
		return current, false, err
	}
	return current, true, nil
}

func (s *Service) setClockLocked(epoch int64) error {
	if s.clockSetter != nil {
		return s.clockSetter(epoch)
	}
	return s.setSystemTimeDirect(epoch)
}

func (s *Service) setSystemTime(timestampStr string) error {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampStr), 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp: %w", err)
	}
	if err := s.setClock(timestamp); err != nil {
		return err
	}
	s.seedNRFTime(timestamp, "explicit Bluetooth time")
	return nil
}

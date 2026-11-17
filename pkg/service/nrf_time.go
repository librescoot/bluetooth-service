package service

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"golang.org/x/sys/unix"
)

const (
	nrfTimeResponseWindow       = 2 * time.Second
	nrfTimePlausibility         = int64(1704067200)
	nrfTimeMaximumHoldoverYears = 5
	trustedTimePollInterval     = 5 * time.Minute
	coordinatorTimeout          = time.Second
)

type clockQuality struct {
	BootID             string  `json:"boot_id"`
	Generation         uint64  `json:"generation"`
	NRFEligible        bool    `json:"nrf_eligible"`
	NRFDeadlineBootNS  int64   `json:"nrf_deadline_boot_ns"`
	External           bool    `json:"external"`
	Outcome            string  `json:"outcome"`
	Preparing          bool    `json:"preparing"`
	Source             string  `json:"source"`
	AgeNS              int64   `json:"age_ns"`
	ResidualSeconds    float64 `json:"residual_seconds"`
	UncertaintySeconds float64 `json:"uncertainty_seconds"`
}
type clockRequest struct {
	Op             string `json:"op"`
	BootID         string `json:"boot_id,omitempty"`
	Generation     uint64 `json:"generation,omitempty"`
	Source         string `json:"source,omitempty"`
	UTCUnixNS      int64  `json:"utc_unix_ns,omitempty"`
	ReceiptBootNS  int64  `json:"receipt_boot_ns,omitempty"`
	DeadlineBootNS int64  `json:"deadline_boot_ns,omitempty"`
	SampleAgeNS    int64  `json:"sample_age_ns,omitempty"`
	UncertaintyNS  int64  `json:"uncertainty_ns,omitempty"`
}
type clockRejection string

func (e clockRejection) Error() string { return "coordinator rejected: " + string(e) }

type clockResponse struct {
	OK      bool         `json:"ok"`
	Error   string       `json:"error"`
	Quality clockQuality `json:"quality"`
}
type userTimeRequest struct {
	receipt int64
	utc     int64
}

type nrfTimeState struct {
	pending         bool
	generation      uint64
	token           uint64
	start, deadline int64
	bootID          string
	clockGeneration uint64
}

func (s *Service) timeNow() time.Time {
	if s.nrfTimeNow != nil {
		return s.nrfTimeNow()
	}
	return time.Now()
}
func (s *Service) boottime() (int64, error) {
	if s.bootNow != nil {
		return s.bootNow()
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, err
	}
	return int64(ts.Sec)*int64(time.Second) + int64(ts.Nsec), nil
}
func (s *Service) nrfTimePlausible(epoch int64, now time.Time) bool {
	return epoch >= nrfTimePlausibility && epoch <= math.MaxInt64/int64(time.Second) && !time.Unix(epoch, 0).After(now.AddDate(nrfTimeMaximumHoldoverYears, 0, 0))
}
func (s *Service) clockCall(req clockRequest) (clockQuality, error) {
	path := s.clockSocket
	if path == "" {
		path = "/run/clock-coordinator/control.sock"
	}
	c, err := net.DialTimeout("unix", path, coordinatorTimeout)
	if err != nil {
		return clockQuality{}, fmt.Errorf("coordinator unavailable: %w", err)
	}
	defer c.Close()
	if err = c.SetDeadline(time.Now().Add(coordinatorTimeout)); err != nil {
		return clockQuality{}, err
	}
	if err = json.NewEncoder(c).Encode(req); err != nil {
		return clockQuality{}, err
	}
	var response clockResponse
	if err = json.NewDecoder(bufio.NewReader(io.LimitReader(c, 4096))).Decode(&response); err != nil {
		return clockQuality{}, fmt.Errorf("coordinator response: %w", err)
	}
	if !response.OK {
		return response.Quality, clockRejection(response.Error)
	}
	if response.Quality.BootID == "" || response.Quality.Generation == 0 {
		return clockQuality{}, fmt.Errorf("invalid coordinator identity")
	}
	return response.Quality, nil
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

func (s *Service) invalidateNRFTimeRequest() {
	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	s.nrfTimeState.generation++
	s.nrfTimeState.pending = false
	s.nrfTimeState.token = 0
}
func (s *Service) requestNRFTime() {
	s.nrfTimeMu.Lock()
	s.nrfTimeState.generation++
	generation := s.nrfTimeState.generation
	s.nrfTimeState.pending = false
	s.nrfTimeMu.Unlock()
	q, err := s.clockCall(clockRequest{Op: "status"})
	if err != nil || !q.NRFEligible {
		s.log.Infof("clock source=nrf decision=reject reason=unavailable_or_ineligible error=%v", err)
		return
	}
	token, err := newNRFTimeToken()
	if err != nil {
		s.log.Warnf("clock source=nrf decision=reject reason=token error=%v", err)
		return
	}
	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	if generation != s.nrfTimeState.generation {
		return
	}
	start, err := s.boottime()
	if err != nil || start >= q.NRFDeadlineBootNS {
		s.log.Infof("clock source=nrf decision=reject reason=recovery_deadline")
		return
	}
	deadline := start + int64(nrfTimeResponseWindow)
	if q.NRFDeadlineBootNS < deadline {
		deadline = q.NRFDeadlineBootNS
	}
	s.nrfTimeState = nrfTimeState{pending: true, generation: generation, token: token, start: start, deadline: deadline, bootID: q.BootID, clockGeneration: q.Generation}
	if err = writeUARTMessageRTCGet(s.GetUSock(), token); err != nil {
		s.nrfTimeState.pending = false
		s.nrfTimeState.token = 0
		s.log.Warnf("clock source=nrf decision=reject reason=uart error=%v", err)
		return
	}
	go func() { time.Sleep(nrfTimeResponseWindow); s.expireNRFTimeRequest(generation) }()
}
func (s *Service) expireNRFTimeRequest(generation uint64) {
	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	if s.nrfTimeState.pending && s.nrfTimeState.generation == generation {
		s.nrfTimeState.pending = false
		s.nrfTimeState.token = 0
	}
}
func (s *Service) handleNRFTimeReply(params map[uint16]interface{}, receipt int64, valid bool) {
	getKey := uint16(ble.TypeRTC) + uint16(ble.TypeRTCGet)
	tokenKey := uint16(ble.TypeRTC) + uint16(ble.TypeRTCToken)
	value, hasValue := params[getKey]
	token, hasToken := params[tokenKey].(uint64)
	if len(params) != 2 || !hasValue || !hasToken || !valid {
		s.log.Infof("clock source=nrf decision=reject reason=malformed_or_no_receipt")
		return
	}
	s.nrfTimeMu.Lock()
	defer s.nrfTimeMu.Unlock()
	st := s.nrfTimeState
	if !st.pending || st.token != token {
		s.log.Infof("clock source=nrf decision=reject reason=stale_token")
		return
	}
	s.nrfTimeState.pending = false
	s.nrfTimeState.token = 0
	nowBoot, err := s.boottime()
	if err != nil || receipt < st.start || receipt >= st.deadline || nowBoot < receipt || nowBoot >= st.deadline {
		s.log.Infof("clock source=nrf decision=reject reason=deadline")
		return
	}
	epoch, ok := nrfTimeValue(value)
	if !ok || !s.nrfTimePlausible(epoch, s.timeNow()) {
		s.log.Infof("clock source=nrf decision=reject reason=implausible")
		return
	}
	// The integer timestamp denotes a one-second interval, and sampling
	// occurred during the UART round trip. Use both interval midpoints.
	roundTrip := receipt - st.start
	if roundTrip > int64(time.Second) {
		s.log.Infof("clock source=nrf decision=reject reason=uart_uncertainty")
		return
	}
	age := roundTrip / 2
	offset := float64(epoch) + 0.5 + float64(age+nowBoot-receipt)/1e9 - float64(s.timeNow().UnixNano())/1e9
	if offset <= 0 {
		s.log.Infof("clock source=nrf decision=reject reason=not_newer age_ns=%d offset_s=%f", age+nowBoot-receipt, offset)
		return
	}
	_, err = s.clockCall(clockRequest{Op: "submit", BootID: st.bootID, Generation: st.clockGeneration, Source: "nrf", UTCUnixNS: epoch*int64(time.Second) + int64(time.Second)/2, ReceiptBootNS: receipt, DeadlineBootNS: st.deadline, SampleAgeNS: age, UncertaintyNS: int64(time.Second)/2 + age})
	if after, clockErr := s.boottime(); clockErr == nil && after >= nowBoot {
		nowBoot = after
		offset = float64(epoch) + 0.5 + float64(age+nowBoot-receipt)/1e9 - float64(s.timeNow().UnixNano())/1e9
	}
	if err != nil {
		decision := "unknown"
		var rejection clockRejection
		if errors.As(err, &rejection) {
			decision = "reject"
		}
		s.log.Infof("clock source=nrf decision=%s reason=coordinator age_ns=%d offset_s=%f error=%v", decision, age+nowBoot-receipt, offset, err)
		return
	}
	s.log.Infof("clock source=nrf decision=accepted age_ns=%d offset_s=%f", age+nowBoot-receipt, offset)
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

func (s *Service) seedNRFTime(epoch int64, source string) bool {
	if !s.nrfTimePlausible(epoch, s.timeNow()) {
		return false
	}
	if err := writeUARTMessage64(s.GetUSock(), ble.TypeRTC, ble.TypeRTCSet, uint64(epoch)); err != nil {
		s.log.Warnf("clock source=%s decision=seed_error error=%v", source, err)
		return false
	}
	s.log.Infof("clock source=%s decision=seeded", source)
	return true
}
func (s *Service) StartTrustedTimeMonitor() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		startup := time.NewTimer(5 * time.Second)
		defer startup.Stop()
		periodic := time.NewTicker(trustedTimePollInterval)
		defer periodic.Stop()
		retry := time.NewTimer(time.Hour)
		retry.Stop()
		defer retry.Stop()
		var retryC <-chan time.Time
		var userRequest userTimeRequest
		for {
			select {
			case <-s.stopCh:
				return
			case <-startup.C:
				s.syncTrustedTime()
			case <-periodic.C:
				s.syncTrustedTime()
			case request := <-s.userTimeRequests:
				if request.receipt < userRequest.receipt {
					continue
				}
				userRequest = request
				if s.syncUserTime(userRequest) {
					retry.Stop()
					retryC = nil
				} else {
					retry.Reset(time.Second)
					retryC = retry.C
				}
			case <-retryC:
				now, err := s.boottime()
				if err != nil || now < userRequest.receipt || now-userRequest.receipt >= int64(30*time.Second) || s.syncUserTime(userRequest) {
					retryC = nil
				} else {
					retry.Reset(time.Second)
				}
			}
		}
	}()
}

func (s *Service) syncTrustedTime() { s.seedFromClockQuality(nil) }

func (s *Service) syncUserTime(request userTimeRequest) bool { return s.seedFromClockQuality(&request) }

func (s *Service) seedFromClockQuality(request *userTimeRequest) bool {
	start, err := s.boottime()
	if err != nil {
		return false
	}
	q, err := s.clockCall(clockRequest{Op: "status"})
	if err != nil {
		s.log.Infof("clock source=unknown decision=seed_reject reason=coordinator error=%v", err)
		return false
	}
	now, err := s.boottime()
	if err != nil || now < start {
		return false
	}
	elapsed := now - start
	maxAge := int64(30 * time.Second)
	if q.Source == "ntp" {
		maxAge = int64(2048 * time.Second)
	}
	if elapsed >= int64(30*time.Second) || q.AgeNS < 0 || q.AgeNS > maxAge || elapsed > maxAge-q.AgeNS {
		s.log.Infof("clock source=%s decision=seed_reject reason=stale", q.Source)
		return false
	}
	age := q.AgeNS + elapsed
	maxError := 1.0
	if q.Source == "gps" || q.Source == "user" {
		maxError = 5
	}
	sourceOK := q.External && (q.Source == "ntp" || q.Source == "gps") || !q.External && q.Source == "user" && q.Outcome != "" && q.Outcome != "unknown" && q.Outcome != "in-flight"
	if request != nil {
		s.userTimeMu.Lock()
		defer s.userTimeMu.Unlock()
		if *request != s.userTimeLatest {
			return false
		}
		if q.Source != "user" || now < request.receipt || now-request.receipt >= int64(30*time.Second) || age > now-request.receipt+int64(2*time.Second) || math.Abs(time.Unix(0, request.utc).Add(time.Duration(now-request.receipt)).Sub(s.timeNow()).Seconds()) > maxError {
			return false
		}
	} else if q.Source == "user" {
		return false
	}
	if q.Preparing || !sourceOK || math.IsNaN(q.ResidualSeconds) || math.IsNaN(q.UncertaintySeconds) || q.UncertaintySeconds < 0 || math.Abs(q.ResidualSeconds)+q.UncertaintySeconds > maxError {
		s.log.Infof("clock source=%s decision=seed_reject reason=quality age_ns=%d offset_s=%f uncertainty_s=%f", q.Source, age, q.ResidualSeconds, q.UncertaintySeconds)
		return false
	}
	return s.seedNRFTime(s.timeNow().Unix(), q.Source)
}
func (s *Service) setSystemTime(timestampStr string, receipt int64) error {
	timestamp, err := strconv.ParseInt(strings.TrimSpace(timestampStr), 10, 64)
	if err != nil || timestamp <= 0 || timestamp > math.MaxInt64/int64(time.Second) {
		return fmt.Errorf("invalid timestamp")
	}
	if receipt < 0 {
		return fmt.Errorf("time receipt unavailable")
	}
	now, err := s.boottime()
	if err != nil || now < receipt || now-receipt >= int64(30*time.Second) {
		return fmt.Errorf("time sample expired")
	}
	q, err := s.clockCall(clockRequest{Op: "status"})
	if err != nil {
		return err
	}
	req := clockRequest{Op: "submit", BootID: q.BootID, Generation: q.Generation, Source: "user", UTCUnixNS: timestamp * int64(time.Second), ReceiptBootNS: receipt, DeadlineBootNS: receipt + int64(30*time.Second), UncertaintyNS: int64(time.Second)}
	now, err = s.boottime()
	if err != nil || now < receipt || now >= req.DeadlineBootNS {
		return fmt.Errorf("time sample expired")
	}
	offset := float64(timestamp) + float64(now-receipt)/1e9 - float64(s.timeNow().UnixNano())/1e9
	s.userTimeMu.Lock()
	defer s.userTimeMu.Unlock()
	_, err = s.clockCall(req)
	if after, clockErr := s.boottime(); clockErr == nil && after >= now {
		now = after
		offset = float64(timestamp) + float64(now-receipt)/1e9 - float64(s.timeNow().UnixNano())/1e9
	}
	if err != nil {
		decision := "unknown"
		var rejection clockRejection
		if errors.As(err, &rejection) {
			decision = "reject"
		}
		s.log.Infof("clock source=user decision=%s age_ns=%d offset_s=%f error=%v", decision, now-receipt, offset, err)
		return err
	}
	s.log.Infof("clock source=user decision=accepted age_ns=%d offset_s=%f", now-receipt, offset)
	s.userTimeLatest = userTimeRequest{receipt: receipt, utc: req.UTCUnixNS}
	if s.userTimeRequests != nil {
		select {
		case s.userTimeRequests <- userTimeRequest{receipt: receipt, utc: req.UTCUnixNS}:
		default:
			select {
			case <-s.userTimeRequests:
			default:
			}
			select {
			case s.userTimeRequests <- userTimeRequest{receipt: receipt, utc: req.UTCUnixNS}:
			default:
			}
		}
	}
	return nil
}

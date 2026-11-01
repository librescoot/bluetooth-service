package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

type testClockSocket struct {
	mu           sync.Mutex
	q            clockQuality
	requests     []clockRequest
	accepted     int
	now          *atomic.Int64
	beforeSubmit func()
	beforeStatus func()
}

func (f *testClockSocket) serve(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clock.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return path
}
func (f *testClockSocket) handle(c net.Conn) {
	defer c.Close()
	var r clockRequest
	if json.NewDecoder(c).Decode(&r) != nil {
		return
	}
	if r.Op == "status" && f.beforeStatus != nil {
		f.beforeStatus()
	}
	if r.Op == "submit" && f.beforeSubmit != nil {
		f.beforeSubmit()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := clockResponse{OK: true, Quality: f.q}
	if r.Op == "submit" {
		f.requests = append(f.requests, r)
		if r.BootID != f.q.BootID || r.Generation != f.q.Generation || (r.Source == "nrf" && !f.q.NRFEligible) || f.now.Load() >= r.DeadlineBootNS {
			out.OK = false
			out.Error = "stale or ineligible"
		} else {
			f.accepted++
		}
	}
	_ = json.NewEncoder(c).Encode(out)
}
func setupClockTest(t *testing.T) (*Service, *mockUSOCK, *testClockSocket, *atomic.Int64) {
	t.Helper()
	s, m := newNRFTimeTestService()
	n := &atomic.Int64{}
	n.Store(int64(10 * time.Second))
	s.bootNow = func() (int64, error) { return n.Load(), nil }
	f := &testClockSocket{q: clockQuality{BootID: "boot", Generation: 1, NRFEligible: true, NRFDeadlineBootNS: int64(100 * time.Second)}, now: n}
	s.clockSocket = f.serve(t)
	return s, m, f, n
}
func testToken(t *testing.T, m *mockUSOCK) uint64 {
	t.Helper()
	var d map[uint16]map[uint16]interface{}
	if err := cbor.Unmarshal(m.lastMessage().data, &d); err != nil {
		t.Fatal(err)
	}
	return d[uint16(ble.TypeRTC)][uint16(ble.TypeRTC)+uint16(ble.TypeRTCToken)].(uint64)
}
func (f *testClockSocket) submitted() []clockRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]clockRequest(nil), f.requests...)
}
func (f *testClockSocket) acceptedCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.accepted }
func deliverRTC(t *testing.T, s *Service, token uint64, receipt int64) {
	t.Helper()
	p := rtcReplyPayload(t, token, uint64(testNow+60))
	p.ReceiptBootNS = receipt
	p.ReceiptValid = true
	s.HandleUSockMessage(p.ID, p)
}
func TestCoordinatorNRFTokenAndTimestamp(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	s.requestNRFTime()
	token := testToken(t, m)
	n.Add(int64(200 * time.Millisecond))
	receipt := n.Load()
	n.Add(int64(300 * time.Millisecond))
	deliverRTC(t, s, token, receipt)
	deliverRTC(t, s, token, receipt)
	got := f.submitted()
	if len(got) != 1 {
		t.Fatalf("submissions %v", got)
	}
	r := got[0]
	if r.Source != "nrf" || r.UTCUnixNS != (testNow+60)*int64(time.Second)+int64(time.Second)/2 || r.SampleAgeNS != int64(100*time.Millisecond) || r.ReceiptBootNS != receipt || r.UncertaintyNS != int64(600*time.Millisecond) {
		t.Fatalf("sample %+v", r)
	}
}
func TestCoordinatorNRFRejectsLateSuspendAndOldSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		advance    time.Duration
		invalidate bool
	}{{"suspend", 2 * time.Second, false}, {"ten seconds", 10 * time.Second, false}, {"twenty seconds", 20 * time.Second, false}, {"old session", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, n := setupClockTest(t)
			s.requestNRFTime()
			token := testToken(t, m)
			receipt := n.Load()
			n.Add(int64(tc.advance))
			if tc.invalidate {
				s.invalidateNRFTimeRequest()
			}
			deliverRTC(t, s, token, receipt)
			if len(f.submitted()) != 0 {
				t.Fatal("stale sample submitted")
			}
		})
	}
}
func TestCoordinatorNRFTransitionAndGeneration(t *testing.T) {
	for _, change := range []string{"external", "generation"} {
		t.Run(change, func(t *testing.T) {
			s, m, f, n := setupClockTest(t)
			s.requestNRFTime()
			token := testToken(t, m)
			f.mu.Lock()
			if change == "external" {
				f.q.NRFEligible = false
			} else {
				f.q.Generation++
			}
			f.mu.Unlock()
			deliverRTC(t, s, token, n.Load())
			got := f.submitted()
			if len(got) != 1 || got[0].Generation != 1 || f.acceptedCount() != 0 {
				t.Fatal(got)
			}
			s.requestNRFTime()
			if change == "external" && m.messageCount() != 1 {
				t.Fatal("offered after external confirmation")
			}
		})
	}
}
func TestCoordinatorDelayedConsumerChecksDeadline(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	s.requestNRFTime()
	token := testToken(t, m)
	receipt := n.Load()
	f.beforeSubmit = func() { n.Add(int64(3 * time.Second)) }
	deliverRTC(t, s, token, receipt)
	if len(f.submitted()) != 1 {
		t.Fatal("expected attempted submission")
	}
	if f.acceptedCount() != 0 {
		t.Fatal("late sample accepted")
	}
	if f.now.Load() < f.submitted()[0].DeadlineBootNS {
		t.Fatal("delay not simulated")
	}
}
func TestCoordinatorUnavailableFailsClosed(t *testing.T) {
	s, m := newNRFTimeTestService()
	s.clockSocket = filepath.Join(t.TempDir(), "missing")
	s.bootNow = func() (int64, error) { return int64(time.Second), nil }
	s.requestNRFTime()
	if m.messageCount() != 0 {
		t.Fatal("request without coordinator")
	}
	if s.setSystemTime("1735689600", int64(time.Second)) == nil {
		t.Fatal("user time accepted without coordinator")
	}
}
func TestCoordinatorUserTimeUTCAndDelay(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range []*time.Location{time.UTC, berlin} {
		t.Run(zone.String(), func(t *testing.T) {
			s, _, f, n := setupClockTest(t)
			s.nrfTimeNow = func() time.Time { return time.Unix(testNow, 0).In(zone) }
			// Both instants are the repeated local 02:30 during the Berlin DST change.
			for _, epoch := range []int64{1729989000, 1729992600} {
				if err := s.setSystemTime(fmt.Sprint(epoch), n.Load()); err != nil {
					t.Fatal(err)
				}
			}
			got := f.submitted()
			if len(got) != 2 || got[0].UTCUnixNS != 1729989000*int64(time.Second) || got[1].UTCUnixNS != 1729992600*int64(time.Second) || got[0].Source != "user" {
				t.Fatal(got)
			}
			old := n.Load()
			n.Add(int64(30 * time.Second))
			if s.setSystemTime("1729989000", old) == nil {
				t.Fatal("stale user timestamp accepted")
			}
		})
	}
}
func TestCoordinatorPeriodicExternalSeeding(t *testing.T) {
	s, m, f, _ := setupClockTest(t)
	for _, q := range []clockQuality{{BootID: "boot", Generation: 1, Source: "ntp", External: true, Preparing: true, AgeNS: int64(time.Second), UncertaintySeconds: .1}, {BootID: "boot", Generation: 1, Source: "local"}, {BootID: "boot", Generation: 1, Source: "nrf", External: true}, {BootID: "boot", Generation: 1, Source: "gps", External: true, AgeNS: int64(31 * time.Second)}, {BootID: "boot", Generation: 1, Source: "ntp", External: true, ResidualSeconds: 1.1}} {
		f.mu.Lock()
		f.q = q
		f.mu.Unlock()
		s.syncTrustedTime()
	}
	if m.messageCount() != 0 {
		t.Fatal("untrusted source seeded")
	}
	f.mu.Lock()
	f.q = clockQuality{BootID: "boot", Generation: 1, Source: "ntp", External: true, AgeNS: int64(time.Second), ResidualSeconds: .05, UncertaintySeconds: .1}
	f.mu.Unlock()
	s.syncTrustedTime()
	s.syncTrustedTime()
	if m.messageCount() != 2 {
		t.Fatalf("healthy periodic seeds %d", m.messageCount())
	}
}

func TestTimeCommandResponseAndLegacySubmission(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	s.handleTimeCommand("set 1735689600", n.Load())
	var response map[uint16]map[uint16]string
	if err := cbor.Unmarshal(m.lastMessage().data, &response); err != nil {
		t.Fatal(err)
	}
	if got := response[uint16(ble.TypeExtended)][uint16(ble.TypeExtended)+uint16(ble.TypeExtendedResponse)]; got != "time:ok" {
		t.Fatal(got)
	}
	s.handleScooterInfoMessage(ble.TypeScooterInfo, uint16(ble.TypeScooterInfo)+uint16(ble.TypeSystemTime), "1735689601", n.Load())
	if got := f.submitted(); len(got) != 2 || got[1].Source != "user" {
		t.Fatal(got)
	}
	s.clockSocket = filepath.Join(t.TempDir(), "missing")
	s.handleTimeCommand("set 1735689600", n.Load())
	if err := cbor.Unmarshal(m.lastMessage().data, &response); err != nil {
		t.Fatal(err)
	}
	if got := response[uint16(ble.TypeExtended)][uint16(ble.TypeExtended)+uint16(ble.TypeExtendedResponse)]; len(got) < 11 || got[:11] != "time:error:" {
		t.Fatal(got)
	}
}
func TestCoordinatorNRFReceiptAndWallJump(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	s.requestNRFTime()
	token := testToken(t, m)
	// A valid receipt delayed in the consumer still uses its original boottime.
	receipt := n.Load()
	n.Add(int64(time.Second))
	s.nrfTimeNow = func() time.Time { return time.Unix(testNow+200, 0) }
	deliverRTC(t, s, token, receipt)
	if len(f.submitted()) != 0 {
		t.Fatal("clock moved ahead before submission")
	}
}

func TestNRFRequestCodecAndTimeoutGeneration(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	s.requestNRFTime()
	first := testToken(t, m)
	s.requestNRFTime()
	second := testToken(t, m)
	if first == 0 || second == 0 || first == second {
		t.Fatal("tokens not unique")
	}
	s.expireNRFTimeRequest(1)
	deliverRTC(t, s, first, n.Load())
	deliverRTC(t, s, second, n.Load())
	if got := f.submitted(); len(got) != 1 {
		t.Fatal(got)
	}
}
func TestNRFRejectsInvalidAndSlowUART(t *testing.T) {
	for _, tc := range []struct {
		name    string
		epoch   uint64
		elapsed time.Duration
	}{{"invalid", 1, 0}, {"roundtrip", uint64(testNow + 60), 1100 * time.Millisecond}, {"past", uint64(testNow - 60), 0}} {
		t.Run(tc.name, func(t *testing.T) {
			s, m, f, n := setupClockTest(t)
			s.requestNRFTime()
			token := testToken(t, m)
			n.Add(int64(tc.elapsed))
			p := rtcReplyPayload(t, token, tc.epoch)
			p.ReceiptValid = true
			p.ReceiptBootNS = n.Load()
			s.HandleUSockMessage(p.ID, p)
			if len(f.submitted()) != 0 {
				t.Fatal("invalid sample submitted")
			}
		})
	}
}

func TestUserSubmissionTimeoutIsUnknownAndNotRetried(t *testing.T) {
	s, _, f, n := setupClockTest(t)
	var output bytes.Buffer
	s.log = logger.NewLogger(log.New(&output, "", 0), logger.LogLevelInfo)
	done := make(chan struct{})
	f.beforeSubmit = func() { time.Sleep(1200 * time.Millisecond); close(done) }
	if err := s.setSystemTime("1735689600", n.Load()); err == nil {
		t.Fatal("timeout reported success")
	}
	if !strings.Contains(output.String(), "decision=unknown") {
		t.Fatal(output.String())
	}
	<-done
	// Wait until the server records the only transmitted submission.
	deadline := time.Now().Add(time.Second)
	for len(f.submitted()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(f.submitted()) != 1 {
		t.Fatal("missing submission or blind retry")
	}
}

func TestDelayedCoordinatorStatusDoesNotSeedOrSubmitOldUserTime(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	f.q = clockQuality{BootID: "boot", Generation: 1, Source: "ntp", External: true, AgeNS: int64(time.Second), UncertaintySeconds: .1}
	f.beforeStatus = func() { n.Add(int64(30 * time.Second)) }
	s.syncTrustedTime()
	if m.messageCount() != 0 {
		t.Fatal("aged status seeded RTC")
	}
	receipt := n.Load()
	if s.setSystemTime("1735689600", receipt) == nil {
		t.Fatal("expired user sample submitted")
	}
	if len(f.submitted()) != 0 {
		t.Fatal("late user sample reached coordinator")
	}
}

func TestNRFRecoveryDeadlineExpiresDuringStatus(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	f.beforeStatus = func() { n.Store(int64(100 * time.Second)) }
	s.requestNRFTime()
	if m.messageCount() != 0 {
		t.Fatal("requested RTC after recovery deadline")
	}
}

func TestNRFSeedingWaitsForPostWakeResume(t *testing.T) {
	s, m, f, n := setupClockTest(t)
	f.mu.Lock()
	f.q = clockQuality{BootID: "boot", Generation: 1, Source: "ntp", External: true, Preparing: true, AgeNS: int64(time.Second), ResidualSeconds: .05, UncertaintySeconds: .1}
	f.mu.Unlock()
	n.Add(5 * int64(time.Second))
	s.syncTrustedTime()
	if m.messageCount() != 0 {
		t.Fatal("pre-resume quality seeded nRF")
	}
	f.mu.Lock()
	f.q.Preparing = false
	f.q.Generation = 2
	f.mu.Unlock()
	s.syncTrustedTime()
	if m.messageCount() != 1 {
		t.Fatal("fresh post-resume quality did not seed nRF")
	}
}

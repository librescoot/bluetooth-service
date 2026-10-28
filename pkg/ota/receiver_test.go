package ota

import (
	"bytes"
	"crypto/sha256"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// fakeWriter records every status frame the receiver sends.
type fakeWriter struct {
	mu     sync.Mutex
	frames [][]byte
}

func (fw *fakeWriter) WriteWithFrameID(frameID byte, data []byte) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	fw.frames = append(fw.frames, cp)
	return nil
}

func (fw *fakeWriter) lastOfOp(op byte) []byte {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	for i := len(fw.frames) - 1; i >= 0; i-- {
		if len(fw.frames[i]) > 0 && fw.frames[i][0] == op {
			return fw.frames[i]
		}
	}
	return nil
}

type fakeInstaller struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (fi *fakeInstaller) Install(component byte, path string) error {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	if fi.err != nil {
		return fi.err
	}
	fi.calls = append(fi.calls, path)
	return nil
}

func newTestReceiver(t *testing.T) (*Receiver, *fakeWriter, *fakeInstaller, string) {
	t.Helper()
	dir := t.TempDir()
	fw := &fakeWriter{}
	fi := &fakeInstaller{}
	lg := logger.NewLogger(log.New(os.Stderr, "", 0), logger.LogLevelError)
	r := New(lg, nil, func() FrameWriter { return fw }, dir, map[byte]Installer{
		ComponentMDB: fi,
	})
	return r, fw, fi, dir
}

func makeBundle(size int) ([]byte, [32]byte) {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	return data, sha256.Sum256(data)
}

func startMsg(data []byte, sha [32]byte, chunk uint16, id string) []byte {
	return EncodeStart(Start{
		Version:   1,
		Component: ComponentMDB,
		ChunkSize: chunk,
		TotalSize: uint32(len(data)),
		SHA256:    sha,
		BundleID:  id,
	})
}

// sendAll pushes the bundle in order, chunked.
func sendAll(r *Receiver, data []byte, chunk int, from int) {
	for off := from; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		r.HandleData(EncodeData(uint32(off), data[off:end]))
	}
}

func TestHappyPath(t *testing.T) {
	r, fw, fi, _ := newTestReceiver(t)
	data, sha := makeBundle(10_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-a"))
	ack := fw.lastOfOp(OpStartAck)
	if ack == nil || ack[1] != StartNew {
		t.Fatalf("expected START_ACK new, got %x", ack)
	}

	sendAll(r, data, 240, 0)
	r.HandleControl([]byte{OpComplete})

	cack := fw.lastOfOp(OpCompleteAck)
	if cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok, got %x", cack)
	}

	fi.mu.Lock()
	defer fi.mu.Unlock()
	if len(fi.calls) != 1 {
		t.Fatalf("installer called %d times", len(fi.calls))
	}
	installed, err := os.ReadFile(fi.calls[0])
	if err != nil {
		t.Fatalf("reading installed bundle: %v", err)
	}
	if !bytes.Equal(installed, data) {
		t.Error("installed bundle differs from source")
	}
}

func TestResumeAfterInterruption(t *testing.T) {
	r, _, _, dir := newTestReceiver(t)
	data, sha := makeBundle(5_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-b"))
	sendAll(r, data[:2400], 240, 0)

	// simulate a crash: new receiver over the same staging dir
	lg := logger.NewLogger(log.New(os.Stderr, "", 0), logger.LogLevelError)
	fw2 := &fakeWriter{}
	fi2 := &fakeInstaller{}
	r2 := New(lg, nil, func() FrameWriter { return fw2 }, dir, map[byte]Installer{ComponentMDB: fi2})

	r2.HandleControl(startMsg(data, sha, 240, "bundle-b"))
	ack := fw2.lastOfOp(OpStartAck)
	if ack == nil || ack[1] != StartResume {
		t.Fatalf("expected START_ACK resume, got %x", ack)
	}
	resume := uint32(ack[2]) | uint32(ack[3])<<8 | uint32(ack[4])<<16 | uint32(ack[5])<<24
	if resume != 2400 {
		t.Fatalf("expected resume offset 2400, got %d", resume)
	}

	sendAll(r2, data, 240, int(resume))
	r2.HandleControl([]byte{OpComplete})
	if cack := fw2.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok after resume, got %x", cack)
	}
}

func TestChangedBundleRestartsFromZero(t *testing.T) {
	r, fw, _, dir := newTestReceiver(t)
	data, sha := makeBundle(3_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-c"))
	sendAll(r, data[:1200], 240, 0)

	// same id, different content
	data2, sha2 := makeBundle(3_001)
	lg := logger.NewLogger(log.New(os.Stderr, "", 0), logger.LogLevelError)
	fw2 := &fakeWriter{}
	r2 := New(lg, nil, func() FrameWriter { return fw2 }, dir, map[byte]Installer{ComponentMDB: &fakeInstaller{}})
	r2.HandleControl(startMsg(data2, sha2, 240, "bundle-c"))

	ack := fw2.lastOfOp(OpStartAck)
	if ack == nil || ack[1] != StartNew {
		t.Fatalf("expected START_ACK new after hash change, got %x", ack)
	}
	_ = fw
	_ = r
}

func TestOutOfOrderTriggersRewind(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	data, sha := makeBundle(2_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-d"))
	r.HandleData(EncodeData(0, data[:240]))
	// skip a chunk
	r.HandleData(EncodeData(480, data[480:720]))

	ack := fw.lastOfOp(OpAck)
	if ack == nil || ack[1]&AckFlagRewind == 0 {
		t.Fatalf("expected REWIND ack, got %x", ack)
	}
	off := uint32(ack[2]) | uint32(ack[3])<<8 | uint32(ack[4])<<16 | uint32(ack[5])<<24
	if off != 240 {
		t.Fatalf("expected rewind to 240, got %d", off)
	}

	// resend from the acked offset; transfer must still complete
	sendAll(r, data, 240, 240)
	r.HandleControl([]byte{OpComplete})
	if cack := fw.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok after rewind, got %x", cack)
	}
}

func TestSHAMismatchDiscardsStaging(t *testing.T) {
	r, fw, _, dir := newTestReceiver(t)
	data, _ := makeBundle(1_000)
	var wrongSHA [32]byte // all zeros

	r.HandleControl(startMsg(data, wrongSHA, 240, "bundle-e"))
	sendAll(r, data, 240, 0)
	r.HandleControl([]byte{OpComplete})

	cack := fw.lastOfOp(OpCompleteAck)
	if cack == nil || cack[1] != CompleteSHAMismatch {
		t.Fatalf("expected COMPLETE_ACK sha mismatch, got %x", cack)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "mdb", "bundle-e*"))
	if len(matches) != 0 {
		t.Errorf("staging not discarded: %v", matches)
	}
}

func TestCompleteSizeMismatchKeepsSession(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	data, sha := makeBundle(1_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-f"))
	sendAll(r, data[:480], 240, 0)
	r.HandleControl([]byte{OpComplete})

	cack := fw.lastOfOp(OpCompleteAck)
	if cack == nil || cack[1] != CompleteSizeMismatch {
		t.Fatalf("expected COMPLETE_ACK size mismatch, got %x", cack)
	}

	// session must survive: sending the rest and completing again succeeds
	sendAll(r, data, 240, 480)
	r.HandleControl([]byte{OpComplete})
	if cack := fw.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok after finishing, got %x", cack)
	}
}

func TestBadStart(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	data, sha := makeBundle(100)

	// unsupported chunk size
	msg := EncodeStart(Start{Version: 1, Component: ComponentMDB, ChunkSize: 512,
		TotalSize: uint32(len(data)), SHA256: sha, BundleID: "x"})
	r.HandleControl(msg)
	if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] != StartBadParams {
		t.Fatalf("oversized chunk accepted: %x", ack)
	}

	// unknown component (no installer registered)
	msg = EncodeStart(Start{Version: 1, Component: ComponentDBC, ChunkSize: 240,
		TotalSize: uint32(len(data)), SHA256: sha, BundleID: "x"})
	r.HandleControl(msg)
	if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] != StartBadParams {
		t.Fatalf("unknown component accepted: %x", ack)
	}
}

func TestQueueFailure(t *testing.T) {
	r, fw, fi, _ := newTestReceiver(t)
	fi.err = os.ErrPermission
	data, sha := makeBundle(500)

	r.HandleControl(startMsg(data, sha, 240, "bundle-g"))
	sendAll(r, data, 240, 0)
	r.HandleControl([]byte{OpComplete})

	if cack := fw.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteQueueFailed {
		t.Fatalf("expected COMPLETE_ACK queue failed, got %x", cack)
	}
}

func TestAbortKeepsStagingForResume(t *testing.T) {
	r, fw, _, dir := newTestReceiver(t)
	data, sha := makeBundle(2_000)

	r.HandleControl(startMsg(data, sha, 240, "bundle-h"))
	sendAll(r, data[:960], 240, 0)
	r.HandleControl([]byte{OpAbort, AbortUserCancel})

	if ack := fw.lastOfOp(OpAbortAck); ack == nil {
		t.Fatal("no ABORT_ACK")
	}
	if _, err := os.Stat(filepath.Join(dir, "mdb", "bundle-h.mender.part")); err != nil {
		t.Errorf("partial file removed on abort: %v", err)
	}

	// resume after abort
	r.HandleControl(startMsg(data, sha, 240, "bundle-h"))
	ack := fw.lastOfOp(OpStartAck)
	if ack == nil || ack[1] != StartResume {
		t.Fatalf("expected resume after abort, got %x", ack)
	}
}

func TestStatusReqIdle(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	r.HandleControl([]byte{OpStatusReq})
	p := fw.lastOfOp(OpInstallProgress)
	if p == nil || p[1] != PhaseIdle {
		t.Fatalf("expected idle phase, got %x", p)
	}
}

func TestDeltaBundleKeepsExtension(t *testing.T) {
	r, fw, fi, dir := newTestReceiver(t)
	data, sha := makeBundle(4_000)
	id := "librescoot-unu-mdb-nightly-20260101T000000.delta"

	r.HandleControl(startMsg(data, sha, 240, id))
	sendAll(r, data, 240, 0)
	r.HandleControl([]byte{OpComplete})

	if cack := fw.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok, got %x", cack)
	}

	fi.mu.Lock()
	defer fi.mu.Unlock()
	if len(fi.calls) != 1 {
		t.Fatalf("installer called %d times", len(fi.calls))
	}
	want := filepath.Join(dir, "mdb", id)
	if fi.calls[0] != want {
		t.Errorf("installer got %q, want %q (extension must survive staging)", fi.calls[0], want)
	}
}

func TestTraversalBundleIDRejected(t *testing.T) {
	r, fw, _, dir := newTestReceiver(t)
	data, sha := makeBundle(100)

	for _, id := range []string{"../evil", "a/b", ".hidden"} {
		r.HandleControl(startMsg(data, sha, 240, id))
		if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] != StartBadParams {
			t.Errorf("bundle id %q accepted: %x", id, ack)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*", "*")); len(matches) != 0 {
		t.Errorf("staging dir not empty after rejected STARTs: %v", matches)
	}
}

// installBundle drives a full transfer + install queueing for retention tests.
func installBundle(t *testing.T, r *Receiver, fw *fakeWriter, id string) {
	t.Helper()
	data, sha := makeBundle(1_000)
	r.HandleControl(startMsg(data, sha, 240, id))
	sendAll(r, data, 240, 0)
	r.HandleControl([]byte{OpComplete})
	if cack := fw.lastOfOp(OpCompleteAck); cack == nil || cack[1] != CompleteOK {
		t.Fatalf("expected COMPLETE_ACK ok, got %x", cack)
	}
}

func TestFinishInstallRetention(t *testing.T) {
	tests := []struct {
		name      string
		id        string
		phase     byte
		wantFinal bool // staged bundle survives finishInstallLocked
	}{
		{"mender success kept as delta base", "bundle-v1", PhasePendingReboot, true},
		{"mender explicit success kept", "bundle-v2", PhaseSuccess, true},
		{"mender failure discarded", "bundle-v3", PhaseFailure, false},
		{"delta success discarded", "librescoot-unu-mdb-nightly-20260101T000000.delta", PhasePendingReboot, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, fw, _, _ := newTestReceiver(t)
			installBundle(t, r, fw, tc.id)

			r.mu.Lock()
			finalPath := r.staging.FinalPath(ComponentMDB, tc.id)
			sidecar := r.staging.sidecarPath(ComponentMDB, tc.id)
			r.lastPhase = tc.phase
			r.finishInstallLocked()
			r.mu.Unlock()

			if _, err := os.Stat(finalPath); (err == nil) != tc.wantFinal {
				t.Errorf("final bundle exists=%v, want %v (err=%v)", err == nil, tc.wantFinal, err)
			}
			if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
				t.Error("sidecar survived finishInstallLocked")
			}
		})
	}
}

// TestDBCPendingRebootClearsToIdle is the regression test for the DBC BLE OTA
// status relay: a DBC install ends with status "pending-reboot" and applies on
// the DBC's next power-on — no MDB reboot happens, so the phase the receiver
// reports must clear back to idle when the DBC's update-service clears
// status:dbc, instead of staying latched at pending-reboot forever.
func TestDBCPendingRebootClearsToIdle(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)

	// Simulate a finished DBC install: update-service reported
	// status:dbc=pending-reboot, the receiver relayed it and finished.
	r.mu.Lock()
	r.installComp = ComponentDBC
	r.lastPhase = PhasePendingReboot
	r.lastPercent = 100
	r.finishInstallLocked()
	r.mu.Unlock()

	if !r.waitingOutcome {
		t.Fatal("expected receiver to keep observing outcome after pending-reboot")
	}

	// The DBC's update-service comes back after the next power-on and clears
	// its status; the receiver must relay idle to the phone and reset state.
	r.onInstallField("dbc", "status:dbc", "idle")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastPhase != PhaseIdle {
		t.Fatalf("lastPhase = %#x, want idle (0x06)", r.lastPhase)
	}
	if r.lastPercent != 0 {
		t.Fatalf("lastPercent = %d, want 0", r.lastPercent)
	}
	if r.waitingOutcome {
		t.Error("waitingOutcome still set after idle")
	}
	frame := fw.lastOfOp(OpInstallProgress)
	if len(frame) < 2 {
		t.Fatalf("no INSTALL_PROGRESS frame sent for idle, got %x", frame)
	}
	if frame[1] != PhaseIdle {
		t.Fatalf("sent phase %#x, want idle (0x06)", frame[1])
	}
}

// TestDBCPendingRebootSurvivesWithoutMDBReset asserts the outcome stays
// pending-reboot (while redis status does too) until the DBC clears it — a
// STATUS_REQ after pending-reboot but before idle must still report it.
func TestDBCPendingRebootKeptUntilIdle(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)

	r.mu.Lock()
	r.installComp = ComponentDBC
	r.lastPhase = PhasePendingReboot
	r.lastPercent = 100
	r.finishInstallLocked()
	r.mu.Unlock()

	// phone reconnects / polls before the DBC powered on again
	r.HandleControl([]byte{OpStatusReq})
	p := fw.lastOfOp(OpInstallProgress)
	if p == nil || p[1] != PhasePendingReboot {
		t.Fatalf("expected pending-reboot before DBC clears, got %x", p)
	}
}

// TestPendingRebootMessagePerComponent checks the OTA_STATUS message text is
// accurate for each component: MDB is rebooted by update-service, the DBC just
// applies on its next power-on.
func TestPendingRebootMessagePerComponent(t *testing.T) {
	tests := []struct {
		comp byte
		want string
	}{
		{ComponentMDB, "reboots after 3 min in stand-by"},
		{ComponentDBC, "applies on next power-on"},
	}
	for _, tc := range tests {
		t.Run(componentName(tc.comp), func(t *testing.T) {
			r, fw, _, _ := newTestReceiver(t)
			r.mu.Lock()
			r.installComp = tc.comp
			r.state = stateInstalling
			r.mu.Unlock()

			r.onInstallField(componentName(tc.comp), "status:"+componentName(tc.comp), "pending-reboot")

			frame := fw.lastOfOp(OpInstallProgress)
			if len(frame) < 4 {
				t.Fatalf("no INSTALL_PROGRESS frame, got %x", frame)
			}
			if got := string(frame[4:]); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIdleResolvedDuringActiveInstall guards against the receiver latching in
// stateInstalling: if update-service clears to "idle" without a terminal outcome
// while we track an install (e.g. it had nothing to install), the receiver must
// return to idle — clearing OTA_STATUS and unblocking new transfers — instead of
// staying stuck on the install phase forever.
func TestIdleResolvedDuringActiveInstall(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	r.mu.Lock()
	r.state = stateInstalling
	r.lastPhase = PhaseInstalling
	r.lastPercent = 42
	r.mu.Unlock()

	r.onInstallField("mdb", "status:mdb", "idle")

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastPhase != PhaseIdle || r.lastPercent != 0 {
		t.Fatalf("idle did not resolve active install to idle: phase=%#x pct=%d", r.lastPhase, r.lastPercent)
	}
	if r.state != stateIdle {
		t.Fatalf("state = %v, want idle", r.state)
	}
	if r.waitingOutcome {
		t.Error("waitingOutcome set by idle during active install")
	}
	if frame := fw.lastOfOp(OpInstallProgress); frame == nil || frame[1] != PhaseIdle {
		t.Fatalf("expected idle frame after resolving, got %x", frame)
	}
}

// TestFailureClearsToIdleAndAllowsNewTransfer is the regression test for the
// "OTA_STATUS stuck" report: a failed install (status:error) must not latch a
// Failure/Installing phase on the characteristic. A reconnecting phone polling
// STATUS_REQ must get idle, and a new transfer must be accepted rather than
// answered with StartInstalling.
func TestFailureClearsToIdleAndAllowsNewTransfer(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)

	// Simulate a delta install that update-service reports as failed.
	r.mu.Lock()
	r.state = stateInstalling
	r.lastPhase = PhaseInstalling
	r.lastPercent = 20
	r.installComp = ComponentMDB
	r.installBundleID = "librescoot-unu-mdb-nightly-20260101T000000.delta"
	r.mu.Unlock()

	r.onInstallField("mdb", "status:mdb", "error")

	// A reconnecting phone polling STATUS_REQ must get idle, not a stale failure.
	r.HandleControl([]byte{OpStatusReq})
	if p := fw.lastOfOp(OpInstallProgress); p == nil || p[1] != PhaseIdle {
		t.Fatalf("STATUS_REQ after failure: got %x, want idle", p)
	}

	// And a brand-new transfer must be accepted, not rejected as busy.
	data, sha := makeBundle(1_000)
	r.HandleControl(startMsg(data, sha, 240, "bundle-new"))
	if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] == StartInstalling {
		t.Fatalf("new START after failure rejected/busy: %x", ack)
	}
}

func TestSameVersionStartRejected(t *testing.T) {
	r, fw, _, _ := newTestReceiver(t)
	r.installedVersion = func(byte) string { return "1.3.0" }
	data, sha := makeBundle(100)

	msg := EncodeStart(Start{Version: 1, Component: ComponentMDB, ChunkSize: 240,
		TotalSize: uint32(len(data)), SHA256: sha, BundleID: "librescoot-unu-mdb-v1.3.0"})
	r.HandleControl(msg)
	if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] != StartAlreadyInstalled {
		t.Fatalf("same-version bundle accepted: %x", ack)
	}
	if r.state != stateIdle {
		t.Fatalf("state = %v, want idle", r.state)
	}

	msg = EncodeStart(Start{Version: 1, Component: ComponentMDB, ChunkSize: 240,
		TotalSize: uint32(len(data)), SHA256: sha, BundleID: "librescoot-unu-mdb-v1.4.0"})
	r.HandleControl(msg)
	if ack := fw.lastOfOp(OpStartAck); ack == nil || ack[1] != StartNew {
		t.Fatalf("newer bundle rejected: %x", ack)
	}
}

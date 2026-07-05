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

func (fw *fakeWriter) last() []byte {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.frames) == 0 {
		return nil
	}
	return fw.frames[len(fw.frames)-1]
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

package filetransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testFrame struct {
	kind byte
	data []byte
}
type testWriter struct{ frames chan testFrame }

func (w *testWriter) WriteWithFrameID(kind byte, p []byte) error {
	w.frames <- testFrame{kind, append([]byte(nil), p...)}
	return nil
}
func fixture(t *testing.T) (*Server, *testWriter, string) {
	t.Helper()
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(logs, 0700); err != nil {
		t.Fatal(err)
	}
	w := &testWriter{make(chan testFrame, 512)}
	s := New(func() Writer { return w }, map[byte]Store{StoreLogs: {Dir: logs, Suffix: ".tar.gz"}, StoreInbox: {Dir: inbox, Writable: true}}, Hooks{})
	t.Cleanup(s.Close)
	return s, w, root
}
func next(t *testing.T, w *testWriter) testFrame {
	t.Helper()
	select {
	case f := <-w.frames:
		return f
	case <-time.After(3 * time.Second):
		t.Fatal("response timeout")
		return testFrame{}
	}
}
func message(t *testing.T, w *testWriter, id uint32) []byte {
	t.Helper()
	var result []byte
	var sequence uint32
	for {
		f := next(t, w)
		if f.kind != FrameStatus || len(f.data) < StatusHeader || len(f.data) > MaxStatusPayload {
			t.Fatal("not a bounded status fragment")
		}
		p := f.data
		if binary.LittleEndian.Uint32(p) != id {
			t.Fatal("wrong request correlation")
		}
		seq := binary.LittleEndian.Uint32(p[4:])
		offset := int(binary.LittleEndian.Uint16(p[8:]))
		total := int(binary.LittleEndian.Uint16(p[10:]))
		if result == nil || seq != sequence {
			result = make([]byte, 0, total)
			sequence = seq
		}
		if offset != len(result) || len(result)+len(p)-StatusHeader > total {
			t.Fatal("invalid fragment order")
		}
		result = append(result, p[StatusHeader:]...)
		if len(result) == total {
			return result
		}
	}
}
func startSession(t *testing.T, s *Server, w *testWriter, r Request) (uint32, uint64) {
	t.Helper()
	s.HandleControl(EncodeRequest(r))
	body := message(t, w, r.ID)
	if len(body) != 58 || body[0] != RespStart || body[1] != OK {
		t.Fatalf("start failed: %x", body)
	}
	return binary.LittleEndian.Uint32(body[2:]), binary.LittleEndian.Uint64(body[6:])
}
func TestUploadVerifyPublishAndResume(t *testing.T) {
	s, w, root := fixture(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 4)
	r := Request{Op: OpPut, ID: 42, Budget: 244, Store: StoreInbox, Name: "sample.bin", Size: uint64(len(data)), Hash: sha256.Sum256(data), Chunk: 16}
	sid, offset := startSession(t, s, w, r)
	if offset != 0 {
		t.Fatal("fresh upload resumed")
	}
	s.HandleData(EncodeData(sid, 0, data[:16]))
	s.HandleControl(EncodeRequest(Request{Op: OpStatus, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, w, r.ID); b[0] != RespAck || binary.LittleEndian.Uint64(b[5:]) != 16 {
		t.Fatalf("offset %x", b)
	}
	if _, err := os.Stat(filepath.Join(root, "inbox", r.Name)); !os.IsNotExist(err) {
		t.Fatal("partial published")
	}
	s.HandleControl(EncodeRequest(Request{Op: OpCancel, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, w, r.ID); b[0] != RespCancel {
		t.Fatal("cancel failed")
	}
	r.ID++
	sid, offset = startSession(t, s, w, r)
	if offset != 16 {
		t.Fatalf("resume=%d", offset)
	}
	s.HandleData(EncodeData(sid-1, 16, data[16:32]))
	for offset < uint64(len(data)) {
		s.HandleData(EncodeData(sid, offset, data[offset:offset+16]))
		offset += 16
	}
	if b := message(t, w, r.ID); b[0] != RespAck || binary.LittleEndian.Uint64(b[5:]) != 64 {
		t.Fatalf("final ack: %x", b)
	}
	complete := Request{Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}
	s.HandleControl(EncodeRequest(complete))
	if b := message(t, w, r.ID); !bytes.Equal(b, []byte{RespComplete, OK}) {
		t.Fatalf("complete %x", b)
	}
	actual, err := os.ReadFile(filepath.Join(root, "inbox", r.Name))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("file mismatch: %v", err)
	}
	s.HandleControl(EncodeRequest(complete))
	if b := message(t, w, r.ID); b[0] != RespComplete {
		t.Fatal("lost completion ACK not retryable")
	}
}
func TestReadOnlyLogsAndChecksumFailure(t *testing.T) {
	s, w, root := fixture(t)
	r := Request{Op: OpPut, ID: 1, Budget: 20, Store: StoreLogs, Name: "logs-test.tar.gz", Size: 8, Chunk: 8}
	s.HandleControl(EncodeRequest(r))
	if b := message(t, w, r.ID); !bytes.Equal(b, []byte{RespError, ErrDenied}) {
		t.Fatalf("read-only write accepted %x", b)
	}
	r.Store = StoreInbox
	r.Name = "bad.bin"
	r.Hash = sha256.Sum256([]byte("different"))
	sid, _ := startSession(t, s, w, r)
	s.HandleData(EncodeData(sid, 0, []byte("12345678")))
	message(t, w, r.ID)
	s.HandleControl(EncodeRequest(Request{Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, w, r.ID); !bytes.Equal(b, []byte{RespError, ErrIntegrity}) {
		t.Fatalf("hash mismatch accepted %x", b)
	}
	if _, err := os.Stat(filepath.Join(root, "inbox", r.Name)); !os.IsNotExist(err) {
		t.Fatal("unverified file published")
	}
	if _, err := os.Stat(filepath.Join(root, "inbox", ".partial", r.Name+".part")); !os.IsNotExist(err) {
		t.Fatal("bad partial retained")
	}
}
func TestDownloadWindowAndRewind(t *testing.T) {
	s, w, root := fixture(t)
	data := bytes.Repeat([]byte("abcdefgh"), 20)
	name := "logs-test.tar.gz"
	if err := os.WriteFile(filepath.Join(root, "logs", name), data, 0600); err != nil {
		t.Fatal(err)
	}
	r := Request{Op: OpGet, ID: 7, Budget: 20, Store: StoreLogs, Name: name, Chunk: 8, Hash: sha256.Sum256(data)}
	sid, _ := startSession(t, s, w, r)
	for i := 0; i < Window; i++ {
		f := next(t, w)
		id, off, p, err := DecodeData(f.data)
		if f.kind != FrameData || err != nil || id != sid || off != uint64(i*8) || !bytes.Equal(p, data[off:off+8]) {
			t.Fatal("invalid download frame")
		}
	}
	select {
	case <-w.frames:
		t.Fatal("sender exceeded window")
	default:
	}
	s.HandleControl(EncodeRequest(Request{Op: OpAck, ID: r.ID, Budget: r.Budget, Session: sid, Offset: 16, Rewind: true}))
	for i := 0; i < Window; i++ {
		f := next(t, w)
		_, off, _, _ := DecodeData(f.data)
		if off != 16+uint64(i*8) {
			t.Fatalf("rewind offset %d", off)
		}
	}
	s.HandleControl(EncodeRequest(Request{Op: OpAck, ID: r.ID, Budget: r.Budget, Session: sid, Offset: 80}))
	for i := 0; i < Window; i++ {
		f := next(t, w)
		_, off, _, _ := DecodeData(f.data)
		if off != 80+uint64(i*8) {
			t.Fatal("ACK did not advance")
		}
	}
	s.HandleControl(EncodeRequest(Request{Op: OpAck, ID: r.ID, Budget: r.Budget, Session: sid, Offset: 144}))
	next(t, w)
	next(t, w)
	s.HandleControl(EncodeRequest(Request{Op: OpAck, ID: r.ID, Budget: r.Budget, Session: sid, Offset: 160}))
	s.HandleControl(EncodeRequest(Request{Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, w, r.ID); b[0] != RespComplete {
		t.Fatal("download completion failed")
	}
}
func TestDisconnectReleasesInhibitorAndRejectsOldSession(t *testing.T) {
	root := t.TempDir()
	w := &testWriter{make(chan testFrame, 32)}
	var mu sync.Mutex
	active := false
	s := New(func() Writer { return w }, map[byte]Store{StoreInbox: {Dir: root, Writable: true}}, Hooks{Active: func(v bool) error { mu.Lock(); active = v; mu.Unlock(); return nil }})
	defer s.Close()
	r := Request{Op: OpPut, ID: 99, Budget: 244, Store: StoreInbox, Name: "item.bin", Size: 64, Chunk: 16}
	sid, _ := startSession(t, s, w, r)
	s.Disconnect()
	deadline := time.Now().Add(time.Second)
	for s.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	held := active
	mu.Unlock()
	if held {
		t.Fatal("disconnect retained inhibitor")
	}
	s.HandleControl(EncodeRequest(Request{Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, w, r.ID); !bytes.Equal(b, []byte{RespError, ErrSession}) {
		t.Fatal("disconnected session survived")
	}
}
func TestStorageConfinementAndListing(t *testing.T) {
	root := t.TempDir()
	store := Store{Dir: root, Suffix: ".tar.gz"}
	external := filepath.Join(t.TempDir(), "secret.tar.gz")
	if err := os.WriteFile(external, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "escape.tar.gz")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret.tar.gz", "/secret.tar.gz", "escape.tar.gz", ".hidden.tar.gz"} {
		if f, err := store.Read(name); err == nil {
			f.close()
			t.Fatalf("opened %s", name)
		}
	}
	for _, name := range []string{"logs-good.tar.gz", "logs-good.tar.gz.part", ".logs-x.part"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := store.List()
	if err != nil || len(files) != 1 || files[0].Name != "logs-good.tar.gz" {
		t.Fatalf("listing %v %v", files, err)
	}
}

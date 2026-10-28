package filetransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestV2OpenRootGoldenResponse(t *testing.T) {
	root := t.TempDir()
	writer := &testWriter{frames: make(chan testFrame, 16)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	defer s.Close()
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	s.idSource = bytes.NewReader(bytes.Repeat([]byte{0xa5}, 16))
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: 1, Budget: 20}))
	want := append([]byte{RespOpenRoot, OK}, bytes.Repeat([]byte{0xa5}, 16)...)
	if got := message(t, writer, 1); !bytes.Equal(got, want) {
		t.Fatalf("OPEN_ROOT response %x", got)
	}
	for len(writer.frames) > 0 {
		f := <-writer.frames
		if len(f.data) > MaxStatusPayload {
			t.Fatalf("oversize STATUS frame %d", len(f.data))
		}
	}
}

func TestLegacyLogsRemainAvailableWithoutDataOptIn(t *testing.T) {
	s, writer, root := fixture(t)
	data := []byte("logs remain available")
	name := "service.tar.gz"
	if err := os.WriteFile(filepath.Join(root, "logs", name), data, 0600); err != nil {
		t.Fatal(err)
	}
	list := Request{Op: OpList, ID: 10, Budget: 244, Store: StoreLogs, Index: 0}
	s.HandleControl(EncodeRequest(list))
	body := message(t, writer, list.ID)
	if len(body) != 2+4+8+8+1+len(name) || body[0] != RespList || body[1] != OK || string(body[23:]) != name {
		t.Fatalf("legacy LOGS list: %x", body)
	}
	stat := Request{Op: OpStat, ID: 11, Budget: 244, Store: StoreLogs, Name: name}
	s.HandleControl(EncodeRequest(stat))
	body = message(t, writer, stat.ID)
	hash := sha256.Sum256(data)
	if len(body) != 42 || body[0] != RespStat || !bytes.Equal(body[10:], hash[:]) {
		t.Fatalf("legacy LOGS stat: %x", body)
	}
	get := Request{Op: OpGet, ID: 12, Budget: 20, Store: StoreLogs, Name: name, Chunk: 8, Hash: hash}
	s.HandleControl(EncodeRequest(get))
	start := message(t, writer, get.ID)
	if len(start) != 58 || start[0] != RespStart {
		t.Fatalf("legacy LOGS GET start: %x", start)
	}
	_, offset, chunk, err := DecodeData(next(t, writer).data)
	if err != nil || offset != 0 || !bytes.Equal(chunk, data[:8]) {
		t.Fatalf("legacy LOGS GET data %x %v", chunk, err)
	}
	read := append([]byte(nil), chunk...)
	for len(read) < len(data) {
		_, _, nextChunk, err := DecodeData(next(t, writer).data)
		if err != nil {
			t.Fatal(err)
		}
		read = append(read, nextChunk...)
	}
	if !bytes.Equal(read, data) {
		t.Fatalf("legacy LOGS GET bytes %q", read)
	}
	s.HandleControl(EncodeRequest(Request{Op: OpCancel, ID: get.ID, Budget: get.Budget, Session: binary.LittleEndian.Uint32(start[2:6])}))
	message(t, writer, get.ID)
	// A v2 request stays unavailable unless the explicit administrative root was enabled.
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: 13, Budget: 20}))
	if got := message(t, writer, 13); !bytes.Equal(got, []byte{RespError, ErrDenied}) {
		t.Fatalf("disabled data browser response %x", got)
	}
}

func TestDataUploadResumesAfterDisconnectAndHidesStaging(t *testing.T) {
	root := t.TempDir()
	writer := &testWriter{frames: make(chan testFrame, 512)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	defer s.Close()
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdef")
	_, node := openRootAndResolve(t, s, writer, "resume.bin", 1)
	r := Request{Version: Version2, Op: OpPutNode, ID: 3, Budget: 20, NodeID: node, Intent: IntentCreate, Chunk: 8, Size: uint64(len(data)), Hash: sha256.Sum256(data)}
	sid, offset := startSession(t, s, writer, r)
	if offset != 0 {
		t.Fatal(offset)
	}
	s.HandleData(EncodeData(sid, 0, data[:8]))
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpStatus, ID: r.ID, Budget: r.Budget, Session: sid}))
	if ack := message(t, writer, r.ID); binary.LittleEndian.Uint64(ack[5:]) != 8 {
		t.Fatalf("partial offset %x", ack)
	}
	s.Disconnect()
	deadline := time.Now().Add(time.Second)
	for s.Busy() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.Busy() {
		t.Fatal("disconnect retained transfer")
	}
	dir, node := openRootAndResolve(t, s, writer, "resume.bin", 4)
	r.ID, r.NodeID = 6, node
	sid, offset = startSession(t, s, writer, r)
	if offset != 8 {
		t.Fatalf("resumed offset %d", offset)
	}
	s.HandleData(EncodeData(sid, offset, data[offset:]))
	message(t, writer, r.ID)
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, writer, r.ID); !bytes.Equal(b, []byte{RespComplete, OK}) {
		t.Fatalf("complete %x", b)
	}
	got, err := os.ReadFile(filepath.Join(root, "resume.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("published %q %v", got, err)
	}
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpListDir, ID: 20, Budget: 244, DirectoryID: dir}))
	listing := message(t, writer, 20)
	if len(listing) < 28 || listing[0] != RespListDir || string(listing[28:]) != "resume.bin" {
		t.Fatalf("staging exposed by LIST: %x", listing)
	}
}

func TestDataReplaceRejectsChangedTarget(t *testing.T) {
	root := t.TempDir()
	name := "target.bin"
	original := []byte("before")
	if err := os.WriteFile(filepath.Join(root, name), original, 0600); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 256)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	defer s.Close()
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	_, node := openRootAndResolve(t, s, writer, name, 1)
	replacement := []byte("verified replacement")
	r := Request{Version: Version2, Op: OpPutNode, ID: 3, Budget: 20, NodeID: node, Intent: IntentReplace, Chunk: 8, Size: uint64(len(replacement)), Hash: sha256.Sum256(replacement), OldSize: uint64(len(original)), OldHash: sha256.Sum256(original)}
	sid, _ := startSession(t, s, writer, r)
	for offset := uint64(0); offset < uint64(len(replacement)); {
		n := min(uint64(8), uint64(len(replacement))-offset)
		s.HandleData(EncodeData(sid, offset, replacement[offset:offset+n]))
		offset += n
	}
	message(t, writer, r.ID)
	changed := []byte("external change")
	if err := os.WriteFile(filepath.Join(root, name), changed, 0600); err != nil {
		t.Fatal(err)
	}
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpComplete, ID: r.ID, Budget: r.Budget, Session: sid}))
	if b := message(t, writer, r.ID); !bytes.Equal(b, []byte{RespError, ErrChanged}) {
		t.Fatalf("changed target response %x", b)
	}
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("changed target lost: %q %v", got, err)
	}
}

func TestDataDirectoryRenameInvalidatesResolvedNode(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("private")
	if err := os.WriteFile(filepath.Join(sub, "data.bin"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 128)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	defer s.Close()
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	_, node := openRootAndResolve(t, s, writer, "nested", 1)
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenDir, ID: 3, Budget: 20, NodeID: node}))
	opened := message(t, writer, 3)
	var nested [16]byte
	copy(nested[:], opened[2:18])
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpResolveName, ID: 4, Budget: 128, DirectoryID: nested, NameOffset: 0, NameTotal: 8, Name: "data.bin"}))
	resolved := message(t, writer, 4)
	var file [16]byte
	copy(file[:], resolved[5:21])
	outside := t.TempDir()
	moved := filepath.Join(outside, "moved")
	if err := os.Rename(sub, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, sub); err != nil {
		t.Fatal(err)
	}
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpStatNode, ID: 5, Budget: 128, NodeID: file}))
	if b := message(t, writer, 5); !bytes.Equal(b, []byte{RespError, ErrChanged}) {
		t.Fatalf("renamed directory response %x", b)
	}
	got, err := os.ReadFile(filepath.Join(outside, "moved", "data.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("outside content changed %q %v", got, err)
	}
}

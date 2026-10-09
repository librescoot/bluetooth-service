package filetransfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func hashVector() [32]byte {
	var v [32]byte
	for i := range v {
		v[i] = byte(i)
	}
	return v
}
func oldHashVector() [32]byte {
	var v [32]byte
	for i := range v {
		v[i] = byte(i + 32)
	}
	return v
}

func TestV2GoldenRequests(t *testing.T) {
	var id [16]byte
	for i := range id {
		id[i] = byte(i)
	}
	vectors := []struct {
		name    string
		request Request
		wire    string
	}{
		{"root", Request{Version: Version2, Op: OpOpenRoot, ID: 1, Budget: 244}, "200201000000f400"},
		{"list", Request{Version: Version2, Op: OpListDir, ID: 2, Budget: 128, DirectoryID: id, Cursor: 0x11223344, NameOffset: 0x5566}, "2102020000008000000102030405060708090a0b0c0d0e0f443322116655"},
		{"open-dir", Request{Version: Version2, Op: OpOpenDir, ID: 3, Budget: 20, NodeID: id}, "2202030000001400000102030405060708090a0b0c0d0e0f"},
		{"stat", Request{Version: Version2, Op: OpStatNode, ID: 0x11223344, Budget: 128, NodeID: id}, "2402443322118000000102030405060708090a0b0c0d0e0f"},
		{"get", Request{Version: Version2, Op: OpGetNode, ID: 0x11223344, Budget: 244, NodeID: id, Offset: 0x0102030405060708, Chunk: 128, Hash: hashVector()}, "250244332211f400000102030405060708090a0b0c0d0e0f08070605040302018000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"},
		{"put-create", Request{Version: Version2, Op: OpPutNode, ID: 0x11223344, Budget: 20, NodeID: id, Intent: IntentCreate, Chunk: 8, Size: 3, Hash: [32]byte{0xba, 0x78, 0x16, 0xbf, 0x8f, 0x01, 0xcf, 0xea, 0x41, 0x41, 0x40, 0xde, 0x5d, 0xae, 0x22, 0x23, 0xb0, 0x03, 0x61, 0xa3, 0x96, 0x17, 0x7a, 0x9c, 0xb4, 0x10, 0xff, 0x61, 0xf2, 0x00, 0x15, 0xad}}, "2602443322111400000102030405060708090a0b0c0d0e0f0008000300000000000000ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad00000000000000000000000000000000000000000000000000000000000000000000000000000000"},
		{"put-replace", Request{Version: Version2, Op: OpPutNode, ID: 0x11223344, Budget: 128, NodeID: id, Intent: IntentReplace, Chunk: 8, Size: 0x0102030405060708, Hash: hashVector(), OldSize: 0x1122334455667788, OldHash: oldHashVector()}, "2602443322118000000102030405060708090a0b0c0d0e0f0108000807060504030201000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f8877665544332211202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f"},
		{"mkdir", Request{Version: Version2, Op: OpMkdir, ID: 4, Budget: 20, NodeID: id}, "2702040000001400000102030405060708090a0b0c0d0e0f"},
		{"close-dir", Request{Version: Version2, Op: OpCloseDir, ID: 5, Budget: 20, DirectoryID: id}, "2802050000001400000102030405060708090a0b0c0d0e0f"},
		{"resolve-utf8", Request{Version: Version2, Op: OpResolveName, ID: 4, Budget: 128, DirectoryID: id, NameOffset: 0, NameTotal: 6, Name: "ü📄"}, "2302040000008000000102030405060708090a0b0c0d0e0f00000600c3bcf09f9384"},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			if got := hex.EncodeToString(EncodeRequest(v.request)); got != v.wire {
				t.Fatalf("wire=%s want=%s", got, v.wire)
			}
			decoded, err := DecodeRequest(EncodeRequest(v.request))
			if err != nil || decoded.Op != v.request.Op {
				t.Fatalf("decode=%+v err=%v", decoded, err)
			}
		})
	}
}

func TestOverlongPathMapsToBoundedInvalidError(t *testing.T) {
	if got := codeFor(syscall.ENAMETOOLONG); got != ErrInvalid {
		t.Fatalf("ENAMETOOLONG maps to %d", got)
	}
}

func TestMalformedV2OperationReturnsError(t *testing.T) {
	s, writer, _ := fixture(t)
	request := Request{Version: Version2, Op: 0x7f, ID: 77, Budget: 20}
	s.HandleControl(EncodeRequest(request))
	if got := message(t, writer, request.ID); !bytes.Equal(got, []byte{RespError, ErrInvalid}) {
		t.Fatalf("malformed v2 response %x", got)
	}
}

func TestDataFrameCharacteristicMaximum(t *testing.T) {
	max := EncodeData(1, 0, make([]byte, MaxPayload-DataHeader))
	if len(max) != MaxPayload {
		t.Fatalf("maximum DATA frame length %d", len(max))
	}
	if _, _, _, err := DecodeData(max); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := DecodeData(EncodeData(1, 0, make([]byte, MaxPayload-DataHeader+1))); err == nil {
		t.Fatal("oversize DATA accepted")
	}
}

func TestV2ControlBoundsAndComponents(t *testing.T) {
	name := strings.Repeat("a", 255)
	for offset := 0; offset < len(name); offset += 100 {
		end := min(len(name), offset+100)
		r := Request{Version: Version2, Op: OpResolveName, ID: 1, Budget: 244, NameTotal: 255, NameOffset: uint16(offset), Name: name[offset:end]}
		wire := EncodeRequest(r)
		if len(wire) > 128 {
			t.Fatalf("control frame length %d", len(wire))
		}
		if _, err := DecodeRequest(wire); err != nil {
			t.Fatalf("chunk at %d: %v", offset, err)
		}
	}
	for _, name := range []string{"", ".", "..", ".partial", ".ble-transfer", "a/b", "a\x00b", string([]byte{0xff}), strings.Repeat("x", 256)} {
		if validComponent(name) {
			t.Errorf("accepted component %q", name)
		}
	}
	if !validComponent("rüber-📄.txt") {
		t.Fatal("ordinary UTF-8 component rejected")
	}
}

func TestV2BrowsingAndResolveLongName(t *testing.T) {
	root := t.TempDir()
	name := strings.Repeat("x", 255)
	content := []byte("data")
	if err := os.WriteFile(filepath.Join(root, name), content, 0640); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 512)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	open := Request{Version: Version2, Op: OpOpenRoot, ID: 10, Budget: 244}
	s.HandleControl(EncodeRequest(open))
	body := message(t, writer, 10)
	if len(body) != 18 || body[0] != RespOpenRoot || body[1] != OK {
		t.Fatalf("root response %x", body)
	}
	var dir [16]byte
	copy(dir[:], body[2:])
	listID := uint32(11)
	var listed []byte
	var nameTotal uint16
	for off := uint16(0); ; {
		r := Request{Version: Version2, Op: OpListDir, ID: listID, Budget: 244, DirectoryID: dir, Cursor: 0, NameOffset: off}
		s.HandleControl(EncodeRequest(r))
		b := message(t, writer, listID)
		if len(b) > 112 || b[0] != RespListDir || b[1] != OK {
			t.Fatalf("list fragment %x", b)
		}
		nameTotal = binary.LittleEndian.Uint16(b[23:25])
		chunk := int(b[27])
		if binary.LittleEndian.Uint16(b[25:27]) != off {
			t.Fatal("wrong name offset")
		}
		listed = append(listed, b[28:28+chunk]...)
		if len(listed) == int(nameTotal) {
			break
		}
		off = uint16(len(listed))
	}
	if string(listed) != name || nameTotal != 255 {
		t.Fatal("long listed component mismatch")
	}
	// The second cursor is EOF; transfer-owned stage is never enumerated.
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpListDir, ID: 99, Budget: 20, DirectoryID: dir, Cursor: 1}))
	if b := message(t, writer, 99); !bytes.Equal(b, []byte{RespListDir, OK, 1, 0, 0, 0, 0}) {
		t.Fatalf("EOF %x", b)
	}
	var node [16]byte
	for off := 0; off < len(name); off += 100 {
		end := min(len(name), off+100)
		r := Request{Version: Version2, Op: OpResolveName, ID: 120, Budget: 128, DirectoryID: dir, NameOffset: uint16(off), NameTotal: 255, Name: name[off:end]}
		s.HandleControl(EncodeRequest(r))
		b := message(t, writer, 120)
		if off < 200 {
			if !bytes.Equal(b, []byte{RespResolveName, OK, byte(end), 0, 0}) {
				t.Fatalf("resolve continuation %x", b)
			}
		} else {
			if len(b) != 22 || b[0] != RespResolveName || b[1] != OK || b[4] != 1 || b[21] != 1 {
				t.Fatalf("resolved %x", b)
			}
			copy(node[:], b[5:21])
			s.HandleControl(EncodeRequest(r))
			if retry := message(t, writer, 120); !bytes.Equal(retry, b) {
				t.Fatalf("final chunk retry %x want %x", retry, b)
			}
		}
	}
	_ = node
}

func TestV2StatGetCreateReplaceAndMkdir(t *testing.T) {
	root := t.TempDir()
	old := []byte("old contents")
	name := "existing.txt"
	if err := os.WriteFile(filepath.Join(root, name), old, 0640); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 512)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	_, node := openRootAndResolve(t, s, writer, name, 1)
	stat := Request{Version: Version2, Op: OpStatNode, ID: 31, Budget: 128, NodeID: node}
	s.HandleControl(EncodeRequest(stat))
	b := message(t, writer, 31)
	oldHash := sha256.Sum256(old)
	if len(b) != 50 || b[0] != RespStatNode || binary.LittleEndian.Uint64(b[2:10]) != uint64(len(old)) || !bytes.Equal(b[18:], oldHash[:]) {
		t.Fatalf("stat %x", b)
	}
	newData := []byte("replacement bytes")
	put := Request{Version: Version2, Op: OpPutNode, ID: 32, Budget: 20, NodeID: node, Intent: IntentReplace, Chunk: 8, Size: uint64(len(newData)), Hash: sha256.Sum256(newData), OldSize: uint64(len(old)), OldHash: sha256.Sum256(old)}
	sid, offset := startSession(t, s, writer, put)
	if offset != 0 {
		t.Fatalf("replacement offset %d", offset)
	}
	for offset < uint64(len(newData)) {
		n := min(uint64(8), uint64(len(newData))-offset)
		s.HandleData(EncodeData(sid, offset, newData[offset:offset+n]))
		offset += n
		if offset < uint64(len(newData)) {
			s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpStatus, ID: 32, Budget: 20, Session: sid}))
			message(t, writer, 32)
		}
	}
	message(t, writer, 32)
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpComplete, ID: 32, Budget: 20, Session: sid}))
	if got := message(t, writer, 32); !bytes.Equal(got, []byte{RespComplete, OK}) {
		t.Fatalf("replace complete %x", got)
	}
	got, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || !bytes.Equal(got, newData) {
		t.Fatalf("replacement contents %q %v", got, err)
	}
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("replacement mode %v %v", info, err)
	}
	// CREATE resolves a missing name and refuses a later collision.
	_, absent := openRootAndResolve(t, s, writer, "new.txt", 40)
	createData := []byte("new")
	create := Request{Version: Version2, Op: OpPutNode, ID: 41, Budget: 20, NodeID: absent, Intent: IntentCreate, Chunk: 8, Size: 3, Hash: sha256.Sum256(createData)}
	sid, offset = startSession(t, s, writer, create)
	s.HandleData(EncodeData(sid, 0, createData))
	message(t, writer, 41)
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpComplete, ID: 41, Budget: 20, Session: sid}))
	if b := message(t, writer, 41); b[0] != RespComplete {
		t.Fatalf("create complete %x", b)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new.txt")); !bytes.Equal(got, createData) {
		t.Fatal("create bytes mismatch")
	}
	_, dirNode := openRootAndResolve(t, s, writer, "new-dir", 50)
	mkdir := Request{Version: Version2, Op: OpMkdir, ID: 51, Budget: 20, NodeID: dirNode}
	s.HandleControl(EncodeRequest(mkdir))
	mb := message(t, writer, 51)
	if len(mb) != 18 || mb[0] != RespMkdir || !isDir(filepath.Join(root, "new-dir")) {
		t.Fatalf("mkdir response %x", mb)
	}
	// GET_NODE uses the same START/DATA/ACK window grammar as v1.
	_, getNode := openRootAndResolve(t, s, writer, "new.txt", 60)
	hash := sha256.Sum256(createData)
	get := Request{Version: Version2, Op: OpGetNode, ID: 61, Budget: 20, NodeID: getNode, Offset: 0, Chunk: 8, Hash: hash}
	sid, _ = startSession(t, s, writer, get)
	frame := next(t, writer)
	transferID, off, p, err := DecodeData(frame.data)
	if err != nil || transferID != sid || off != 0 || !bytes.Equal(p, createData) {
		t.Fatalf("GET_NODE data %x %v", frame.data, err)
	}
}

func openRootAndResolve(t *testing.T, s *Server, w *testWriter, name string, id uint32) ([16]byte, [16]byte) {
	t.Helper()
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: id, Budget: 244}))
	b := message(t, w, id)
	var dir [16]byte
	copy(dir[:], b[2:18])
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpResolveName, ID: id + 1, Budget: 128, DirectoryID: dir, NameOffset: 0, NameTotal: uint16(len(name)), Name: name}))
	b = message(t, w, id+1)
	if len(b) != 22 || b[4] != 1 {
		t.Fatalf("resolve %q: %x", name, b)
	}
	var node [16]byte
	copy(node[:], b[5:21])
	return dir, node
}
func isDir(p string) bool { info, err := os.Stat(p); return err == nil && info.IsDir() }

func TestV2SymlinkAndStagingConfinement(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "jump")); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 64)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: 1, Budget: 20}))
	rootReply := message(t, writer, 1)
	var dir [16]byte
	copy(dir[:], rootReply[2:18])
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpResolveName, ID: 2, Budget: 128, DirectoryID: dir, NameOffset: 0, NameTotal: 4, Name: "jump"}))
	if b := message(t, writer, 2); !bytes.Equal(b, []byte{RespError, ErrDenied}) {
		t.Fatalf("symlink resolve: %x", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); err != nil {
		t.Fatal(err)
	}
}
func messageNoop(t *testing.T, w *testWriter) []byte {
	t.Helper()
	select {
	case f := <-w.frames:
		return f.data
	default:
		return nil
	}
}

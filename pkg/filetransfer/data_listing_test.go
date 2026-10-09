package filetransfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirectoryMutationInvalidatesContinuationButAllowsRefresh(t *testing.T) {
	root := t.TempDir()
	name := strings.Repeat("x", 255)
	if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	server, writer, _ := fixture(t)
	if err := server.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	server.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: 1, Budget: 20}))
	rootReply := message(t, writer, 1)
	var dir [16]byte
	copy(dir[:], rootReply[2:18])
	first := Request{Version: Version2, Op: OpListDir, ID: 10, Budget: 244, DirectoryID: dir, Cursor: 0}
	server.HandleControl(EncodeRequest(first))
	body := message(t, writer, first.ID)
	if body[0] != RespListDir || body[27] != 84 {
		t.Fatalf("first name fragment %x", body)
	}
	if err := os.WriteFile(filepath.Join(root, "new-entry"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	continuation := first
	continuation.NameOffset = 84
	server.HandleControl(EncodeRequest(continuation))
	if got := message(t, writer, first.ID); !bytes.Equal(got, []byte{RespError, ErrChanged}) {
		t.Fatalf("stale name fragment %x", got)
	}
	// A new cursor-zero request starts a fresh listing on the same directory handle.
	refresh := Request{Version: Version2, Op: OpListDir, ID: 11, Budget: 244, DirectoryID: dir, Cursor: 0}
	server.HandleControl(EncodeRequest(refresh))
	if got := message(t, writer, refresh.ID); len(got) < 29 || got[0] != RespListDir || got[1] != OK {
		t.Fatalf("refresh after add %x", got)
	}
	if err := os.Remove(filepath.Join(root, "new-entry")); err != nil {
		t.Fatal(err)
	}
	refresh.ID++
	server.HandleControl(EncodeRequest(refresh))
	if got := message(t, writer, refresh.ID); len(got) < 29 || got[0] != RespListDir || got[1] != OK {
		t.Fatalf("refresh after remove %x", got)
	}
}

func TestDirectoryMutationBetweenPagesReturnsChanged(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server, writer, _ := fixture(t)
	if err := server.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	dir, _ := openRootAndResolve(t, server, writer, "a.txt", 1)
	first := Request{Version: Version2, Op: OpListDir, ID: 10, Budget: 244, DirectoryID: dir, Cursor: 0}
	server.HandleControl(EncodeRequest(first))
	if body := message(t, writer, first.ID); body[0] != RespListDir || string(body[28:]) != "a.txt" {
		t.Fatalf("first page %x", body)
	}
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("c"), 0600); err != nil {
		t.Fatal(err)
	}
	next := Request{Version: Version2, Op: OpListDir, ID: 11, Budget: 244, DirectoryID: dir, Cursor: 1}
	server.HandleControl(EncodeRequest(next))
	if got := message(t, writer, next.ID); !bytes.Equal(got, []byte{RespError, ErrChanged}) {
		t.Fatalf("stale next page %x", got)
	}
	// The same directory handle can be refreshed with cursor 0 and a new request ID.
	first.ID = 12
	server.HandleControl(EncodeRequest(first))
	if body := message(t, writer, first.ID); body[0] != RespListDir || body[1] != OK {
		t.Fatalf("fresh first page %x", body)
	}
}

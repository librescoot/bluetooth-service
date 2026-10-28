package filetransfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestDirectoryListingStreamsSequentialCursorsAndFragments(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{strings.Repeat("a", 254) + "1", strings.Repeat("a", 254) + "2"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	server, writer, _ := fixture(t)
	if err := server.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	server.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpOpenRoot, ID: 1, Budget: 20}))
	rootReply := message(t, writer, 1)
	var dir [16]byte
	copy(dir[:], rootReply[2:18])
	first := Request{Version: Version2, Op: OpListDir, ID: 10, Budget: 244, DirectoryID: dir}
	server.HandleControl(EncodeRequest(first))
	if body := message(t, writer, first.ID); body[0] != RespListDir || body[27] != 84 {
		t.Fatalf("first fragment %x", body)
	}
	iterator := server.listState.iterator
	if iterator == nil || server.listState.position != 1 {
		t.Fatalf("iterator=%v position=%d", iterator, server.listState.position)
	}
	for _, offset := range []uint16{84, 168, 252} {
		part := first
		part.NameOffset = offset
		server.HandleControl(EncodeRequest(part))
		if body := message(t, writer, part.ID); body[0] != RespListDir || server.listState.iterator != iterator || server.listState.position != 1 {
			t.Fatalf("fragment %d lost iterator position: %x", offset, body)
		}
	}
	next := Request{Version: Version2, Op: OpListDir, ID: 11, Budget: 244, DirectoryID: dir, Cursor: 1}
	server.HandleControl(EncodeRequest(next))
	if body := message(t, writer, next.ID); body[0] != RespListDir || body[27] != 84 || server.listState.iterator != iterator || server.listState.position != 2 {
		t.Fatalf("second cursor did not continue iterator: %x", body)
	}
	end := Request{Version: Version2, Op: OpListDir, ID: 12, Budget: 244, DirectoryID: dir, Cursor: 2}
	server.HandleControl(EncodeRequest(end))
	if body := message(t, writer, end.ID); !bytes.Equal(body, []byte{RespListDir, OK, 2, 0, 0, 0, 0}) || server.listState.iterator != nil {
		t.Fatalf("EOF did not close iterator: %x", body)
	}
}

func TestDirectoryListingClosesIteratorOnResetCancelAndExpiry(t *testing.T) {
	for _, cleanup := range []string{"reset", "cancel", "expiry"} {
		t.Run(cleanup, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "entry"), []byte("x"), 0600); err != nil {
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
			list := Request{Version: Version2, Op: OpListDir, ID: 10, Budget: 244, DirectoryID: dir}
			server.HandleControl(EncodeRequest(list))
			message(t, writer, list.ID)
			iterator := server.listState.iterator
			if iterator == nil {
				t.Fatal("listing did not retain iterator")
			}
			switch cleanup {
			case "reset":
				server.resetDataHandles()
			case "cancel":
				server.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpCancel, ID: list.ID, Budget: list.Budget}))
				message(t, writer, list.ID)
			case "expiry":
				server.listState.touched = time.Now().Add(-11 * time.Minute)
				server.expireDataHandles()
			}
			if server.listState != nil {
				t.Fatal("listing state retained after cleanup")
			}
			if _, err := iterator.Stat(); err == nil {
				t.Fatal("iterator remained open")
			}
		})
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

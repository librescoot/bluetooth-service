package filetransfer

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func testStage(t *testing.T, root string, n int, modified time.Time) string {
	t.Helper()
	path := filepath.Join(root, ".ble-transfer", fmt.Sprintf("%064x.part", n))
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDataStageCleanupExpiresAndEvictsOldest(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ble-transfer"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := testStage(t, root, 1, now.Add(-maxDataPartialAge-time.Hour))
	fresh := testStage(t, root, 2, now.Add(-time.Hour))
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := cleanDataStages(r, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale still present: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh lost: %v", err)
	}
	for i := 3; i <= maxDataPartialFiles+1; i++ {
		testStage(t, root, i, now)
	}
	if err := cleanDataStages(r, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest retained: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".ble-transfer"))
	if err != nil || len(entries) != maxDataPartialFiles-1 {
		t.Fatalf("remaining %d, err %v", len(entries), err)
	}
}

func TestDataStageCleanupNeverDeletesUnknownOrSymlinks(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, ".ble-transfer")
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stage, fmt.Sprintf("%064x.part", 1))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxDataPartialFiles-1; i++ {
		if err := os.WriteFile(filepath.Join(stage, fmt.Sprintf("unknown-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := cleanDataStages(r, time.Now()); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected ENOSPC: %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "keep" {
		t.Fatalf("target %q, %v", got, err)
	}
}

func TestReclaimSpaceProtectsCurrentAndImpossibleUploads(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ble-transfer"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	current := testStage(t, root, 1, now.Add(-2*time.Hour))
	oldest := testStage(t, root, 2, now.Add(-time.Hour))
	newest := testStage(t, root, 3, now)
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := reclaimDataSpace(r, root, filepath.Base(current), ^uint64(0), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("impossible upload: %v", err)
	}
	if _, err := os.Stat(oldest); err != nil {
		t.Fatalf("impossible upload discarded a partial: %v", err)
	}
	if err := reclaimDataSpace(r, root, filepath.Base(current), 1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest retained: %v", err)
	}
	for _, path := range []string{current, newest} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected partial lost: %v", err)
		}
	}
}

func TestDataUploadCanStartAfterPriorQuotaIsFilled(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".ble-transfer"), 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i := 0; i < maxDataPartialFiles; i++ {
		testStage(t, root, i, now.Add(time.Duration(i)*time.Millisecond))
	}
	s := New(nil, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetDataRoot(root); err != nil {
		t.Fatal(err)
	}
	data := []byte("new upload")
	req := Request{Intent: IntentCreate, Size: uint64(len(data)), Hash: sha256.Sum256(data), Chunk: 8}
	f, offset, err := s.beginDataWrite(req, "new.mender", func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	if offset != 0 {
		t.Fatalf("offset %d", offset)
	}
	entries, err := os.ReadDir(filepath.Join(root, ".ble-transfer"))
	if err != nil || len(entries) != maxDataPartialFiles {
		t.Fatalf("remaining %d, err %v", len(entries), err)
	}
}

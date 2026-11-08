package ota

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

func testDBCInstaller(t *testing.T, stagingDir string) *DBCInstaller {
	t.Helper()
	return &DBCInstaller{
		Log:        logger.NewLogger(log.New(os.Stderr, "", 0), logger.LogLevelError),
		StagingDir: stagingDir,
	}
}

// Only installable artifacts are copied to the dashboard: a partial transfer or
// an unrelated file in the staging directory must not be shipped.
func TestStagedArtifactsListsOnlyArtifacts(t *testing.T) {
	dir := t.TempDir()
	dbcDir := filepath.Join(dir, "dbc")
	if err := os.MkdirAll(dbcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"librescoot-unu-dbc-v1.4.0.mender",
		"librescoot-unu-dbc-v1.4.1.delta",
		"librescoot-unu-dbc-nightly-20260101T000000.hdelta",
		"librescoot-unu-dbc-v1.5.0.mender.part",
		".hidden.mender",
		"notes.txt",
	} {
		if err := os.WriteFile(filepath.Join(dbcDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := testDBCInstaller(t, dir).stagedArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.0.mender"):                  true,
		filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.1.delta"):                   true,
		filepath.Join(dbcDir, "librescoot-unu-dbc-nightly-20260101T000000.hdelta"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("stagedArtifacts = %v, want %d entries", got, len(want))
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected staged artifact %s", path)
		}
	}
}

func TestPruneStagedRemovesOnlyArtifacts(t *testing.T) {
	dir := t.TempDir()
	dbcDir := filepath.Join(dir, "dbc")
	if err := os.MkdirAll(dbcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.0.mender")
	partial := filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.0.mender.part")
	for _, path := range []string{artifact, partial} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testDBCInstaller(t, dir).PruneStaged(ComponentDBC)

	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact survived cleanup: %v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial transfer was removed: %v", err)
	}
}

func TestStagedArtifactsMissingDirectoryIsEmpty(t *testing.T) {
	got, err := testDBCInstaller(t, t.TempDir()).stagedArtifacts()
	if err != nil || len(got) != 0 {
		t.Fatalf("stagedArtifacts = %v, %v", got, err)
	}
}

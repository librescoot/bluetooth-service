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
	dbcDir := filepath.Join(dir, "ble", "dbc")
	if err := os.MkdirAll(dbcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(dir, "dbc")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyBase := filepath.Join(legacyDir, "librescoot-unu-dbc-v1.4.0.mender")
	if err := os.WriteFile(legacyBase, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"librescoot-unu-dbc-v1.4.0.mender", // unmarked retained base
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
	for _, name := range []string{
		"librescoot-unu-dbc-v1.4.1.delta",
		"librescoot-unu-dbc-nightly-20260101T000000.hdelta",
	} {
		if err := os.WriteFile(filepath.Join(dbcDir, name+stagedMarkerSuffix), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := testDBCInstaller(t, dir).stagedArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.1.delta"):                   true,
		filepath.Join(dbcDir, "librescoot-unu-dbc-nightly-20260101T000000.hdelta"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("stagedArtifacts = %v, want %d entries", got, len(want))
	}
	if _, err := os.Stat(legacyBase); err != nil {
		t.Fatalf("legacy base unexpectedly changed during listing: %v", err)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected staged artifact %s", path)
		}
	}
}

func TestPruneStagedRemovesOnlyArtifacts(t *testing.T) {
	dir := t.TempDir()
	dbcDir := filepath.Join(dir, "ble", "dbc")
	if err := os.MkdirAll(dbcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(dir, "dbc")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyArtifact := filepath.Join(legacyDir, "old-base.mender")
	if err := os.WriteFile(legacyArtifact, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.0.mender")
	marker := artifact + stagedMarkerSuffix
	partial := filepath.Join(dbcDir, "librescoot-unu-dbc-v1.4.0.mender.part")
	for _, path := range []string{artifact, marker, partial} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testDBCInstaller(t, dir).PruneStaged(ComponentDBC)

	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("artifact survived cleanup: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("staged marker survived cleanup: %v", err)
	}
	if _, err := os.Stat(legacyArtifact); !os.IsNotExist(err) {
		t.Fatalf("legacy DBC artifact survived cleanup: %v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial transfer was removed: %v", err)
	}
}

func TestStageMarksOnlyTheCompletedBLEArtifact(t *testing.T) {
	dir := t.TempDir()
	dbcDir := filepath.Join(dir, "ble", "dbc")
	if err := os.MkdirAll(dbcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dbcDir, "librescoot-unu-dbc-nightly-20260101T000000.hdelta")
	if err := os.WriteFile(artifact, []byte("patch"), 0o644); err != nil {
		t.Fatal(err)
	}

	installer := testDBCInstaller(t, dir)
	if err := installer.Stage(ComponentDBC, artifact); err != nil {
		t.Fatal(err)
	}
	if err := installer.Stage(ComponentDBC, filepath.Join(dbcDir, "../not-in-staging.mender")); err == nil {
		t.Fatal("Stage accepted an artifact outside the DBC staging directory")
	}

	got, err := installer.stagedArtifacts()
	if err != nil || len(got) != 1 || got[0] != artifact {
		t.Fatalf("stagedArtifacts = %v, %v; want only %s", got, err, artifact)
	}
}

func TestStagedArtifactsMissingDirectoryIsEmpty(t *testing.T) {
	got, err := testDBCInstaller(t, t.TempDir()).stagedArtifacts()
	if err != nil || len(got) != 0 {
		t.Fatalf("stagedArtifacts = %v, %v", got, err)
	}
}

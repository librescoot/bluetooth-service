package ota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStagedNamePaths(t *testing.T) {
	st := NewStaging("/base")

	tests := []struct {
		bundleID  string
		wantFinal string
	}{
		{"bundle-v1", "/base/mdb/bundle-v1.mender"},
		{"librescoot-unu-mdb-nightly-20260101T000000.delta", "/base/mdb/librescoot-unu-mdb-nightly-20260101T000000.delta"},
		{"already.mender", "/base/mdb/already.mender"},
	}
	for _, tc := range tests {
		if got := st.FinalPath(ComponentMDB, tc.bundleID); got != tc.wantFinal {
			t.Errorf("FinalPath(%q) = %q, want %q", tc.bundleID, got, tc.wantFinal)
		}
		if got, want := st.PartPath(ComponentMDB, tc.bundleID), tc.wantFinal+".part"; got != want {
			t.Errorf("PartPath(%q) = %q, want %q", tc.bundleID, got, want)
		}
	}

	if got := st.FinalPath(ComponentDBC, "x.delta"); got != "/base/dbc/x.delta" {
		t.Errorf("DBC FinalPath = %q", got)
	}
}

func TestForgetKeepsFinalBundle(t *testing.T) {
	dir := t.TempDir()
	st := NewStaging(dir)
	id := "bundle-v1"

	if err := st.WriteSidecar(ComponentMDB, &Sidecar{BundleID: id}); err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	if err := os.WriteFile(st.PartPath(ComponentMDB, id), []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(st.FinalPath(ComponentMDB, id), []byte("final"), 0o644); err != nil {
		t.Fatal(err)
	}

	st.Forget(ComponentMDB, id)

	if _, err := os.Stat(st.FinalPath(ComponentMDB, id)); err != nil {
		t.Errorf("final bundle removed by Forget: %v", err)
	}
	if _, err := os.Stat(st.PartPath(ComponentMDB, id)); !os.IsNotExist(err) {
		t.Error("part file survived Forget")
	}
	if sc := st.LoadSidecar(ComponentMDB, id); sc != nil {
		t.Error("sidecar survived Forget")
	}
}

func TestCleanupStaleBothExtensions(t *testing.T) {
	dir := t.TempDir()
	st := NewStaging(dir)
	mdbDir := filepath.Join(dir, "mdb")
	if err := os.MkdirAll(mdbDir, 0o755); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-8 * 24 * time.Hour)
	write := func(name string) string {
		p := filepath.Join(mdbDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	age := func(p string) {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	staleMender := write("stale.mender.part")
	age(staleMender)
	staleMenderSidecar := write("stale.json")
	staleDelta := write("stale2.delta.part")
	age(staleDelta)
	staleDeltaSidecar := write("stale2.delta.json")
	freshDelta := write("fresh.delta.part")
	keptFinal := write("base.mender") // owned by update-service, must survive
	age(keptFinal)

	if removed := st.CleanupStale(); removed != 2 {
		t.Errorf("CleanupStale removed %d, want 2", removed)
	}
	for _, gone := range []string{staleMender, staleMenderSidecar, staleDelta, staleDeltaSidecar} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived cleanup", filepath.Base(gone))
		}
	}
	for _, kept := range []string{freshDelta, keptFinal} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s removed by cleanup: %v", filepath.Base(kept), err)
		}
	}
}

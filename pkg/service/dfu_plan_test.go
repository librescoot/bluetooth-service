package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindLatestZipPair(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) { writeTestFile(t, dir, name) }
	touch("nrf-fw-bl-v2.0.0-ls.zip")
	touch("nrf-fw-app-v2.0.0-ls.zip")
	touch("nrf-fw-bl-v2.1.0-ls.zip")
	touch("nrf-fw-app-v2.1.0-ls.zip")
	touch("nrf-fw-full-v2.1.0-ls.zip") // ignored
	touch("nrfupdate.py")              // ignored

	pair, err := FindLatestZipPair(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair == nil {
		t.Fatal("expected a pair, got nil")
	}
	if pair.Version != "v2.1.0-ls" {
		t.Errorf("version: got %q, want v2.1.0-ls", pair.Version)
	}
	if filepath.Base(pair.BLPath) != "nrf-fw-bl-v2.1.0-ls.zip" {
		t.Errorf("bl path: got %q", pair.BLPath)
	}
	if filepath.Base(pair.AppPath) != "nrf-fw-app-v2.1.0-ls.zip" {
		t.Errorf("app path: got %q", pair.AppPath)
	}
}

func TestFindLatestZipPair_SkipsIncompletePairs(t *testing.T) {
	dir := t.TempDir()
	// Only an app zip for the newer version — should fall back to the older
	// version that has both pieces.
	writeTestFile(t, dir, "nrf-fw-bl-v2.0.0-ls.zip")
	writeTestFile(t, dir, "nrf-fw-app-v2.0.0-ls.zip")
	writeTestFile(t, dir, "nrf-fw-app-v2.1.0-ls.zip")

	pair, err := FindLatestZipPair(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair == nil {
		t.Fatal("expected v2.0.0-ls fallback, got nil")
	}
	if pair.Version != "v2.0.0-ls" {
		t.Errorf("got %q, want v2.0.0-ls", pair.Version)
	}
}

func TestFindLatestZipPair_EmptyDir(t *testing.T) {
	pair, err := FindLatestZipPair(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pair != nil {
		t.Errorf("expected nil pair, got %+v", pair)
	}
}

func TestFindLatestZipPair_MissingDir(t *testing.T) {
	pair, err := FindLatestZipPair(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("missing dir should not be an error, got: %v", err)
	}
	if pair != nil {
		t.Errorf("expected nil pair, got %+v", pair)
	}
}

// writeTestFile creates a fixture file for zip-pair discovery tests; its
// content is irrelevant, only the name and presence matter.
func writeTestFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildDFUPlan_BLUpdateNeeded(t *testing.T) {
	pair := &ZipPair{
		Version: "v2.1.0-ls",
		BLPath:  "/fw/nrf-fw-bl-v2.1.0-ls.zip",
		AppPath: "/fw/nrf-fw-app-v2.1.0-ls.zip",
	}
	plan := BuildDFUPlan(pair, 3, 4, 2)
	if len(plan.Stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(plan.Stages))
	}
	if plan.Stages[0].Label != "bootloader+softdevice" {
		t.Errorf("stage 0 label: %q", plan.Stages[0].Label)
	}
	if plan.Stages[0].Path != pair.BLPath {
		t.Errorf("stage 0 path: %q", plan.Stages[0].Path)
	}
	if plan.Stages[1].Label != "application" {
		t.Errorf("stage 1 label: %q", plan.Stages[1].Label)
	}
	if plan.Stages[1].Path != pair.AppPath {
		t.Errorf("stage 1 path: %q", plan.Stages[1].Path)
	}
}

func TestBuildDFUPlan_BLAlreadyCurrent(t *testing.T) {
	pair := &ZipPair{
		Version: "v2.1.0-ls",
		BLPath:  "/fw/nrf-fw-bl-v2.1.0-ls.zip",
		AppPath: "/fw/nrf-fw-app-v2.1.0-ls.zip",
	}
	plan := BuildDFUPlan(pair, 4, 4, 2)
	if len(plan.Stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(plan.Stages))
	}
	if plan.Stages[0].Label != "application" {
		t.Errorf("stage 0 label: %q", plan.Stages[0].Label)
	}
}

func TestBuildDFUPlan_BLAheadOfTarget(t *testing.T) {
	pair := &ZipPair{
		Version: "v2.1.0-ls",
		BLPath:  "/fw/nrf-fw-bl-v2.1.0-ls.zip",
		AppPath: "/fw/nrf-fw-app-v2.1.0-ls.zip",
	}
	// Installed BL is newer than what we're shipping — skip the BL stage.
	// App still flashes because the caller wouldn't have reached here unless
	// the user-facing version changed.
	plan := BuildDFUPlan(pair, 5, 4, 2)
	if len(plan.Stages) != 1 {
		t.Fatalf("got %d stages, want 1", len(plan.Stages))
	}
	if plan.Stages[0].Label != "application" {
		t.Errorf("stage 0 label: %q", plan.Stages[0].Label)
	}
}

func TestDFUPlanSummary(t *testing.T) {
	cases := []struct {
		name string
		plan DFUPlan
		want string
	}{
		{
			name: "up to date",
			plan: DFUPlan{Version: "v2.1.0-ls"},
			want: "v2.1.0-ls: up to date",
		},
		{
			name: "app only",
			plan: DFUPlan{
				Version: "v2.1.0-ls",
				Stages:  []DFUStage{{Label: "application"}},
			},
			want: "v2.1.0-ls: application",
		},
		{
			name: "bl then app",
			plan: DFUPlan{
				Version: "v2.1.0-ls",
				Stages: []DFUStage{
					{Label: "bootloader+softdevice"},
					{Label: "application"},
				},
			},
			want: "v2.1.0-ls: bootloader+softdevice → application",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.Summary(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

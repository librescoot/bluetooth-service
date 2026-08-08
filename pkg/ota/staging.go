package ota

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// DefaultStagingDir is where partial and completed bundles are staged. It lives
// on the persistent /data partition so transfers survive reboots. The
// per-component subdirectories (/data/ota/mdb, /data/ota/dbc) are
// update-service's own download directories — staging there means a completed
// full image is found by update-service's base lookup for later delta updates,
// and keeps the files inside the dirs ums-service's orphan sweep leaves alone.
const DefaultStagingDir = "/data/ota"

// freeSpaceMargin is kept free on top of the bundle size so the staging never
// starves update-service's own /data usage.
const freeSpaceMargin = 32 << 20 // 32 MiB

// staleAge is how long an untouched .part file survives before cleanup.
const staleAge = 7 * 24 * time.Hour

// Sidecar records the transfer identity next to the .part file; a START whose
// identity does not match restarts the transfer from zero.
type Sidecar struct {
	BundleID  string `json:"bundle_id"`
	Component string `json:"component"`
	TotalSize uint32 `json:"total_size"`
	SHA256    string `json:"sha256"`
	ChunkSize uint16 `json:"chunk_size"`
	CreatedAt string `json:"created_at"`
}

// Staging manages the on-disk layout: <dir>/<component>/<stagedName>.part plus
// <bundleID>.json sidecars, where stagedName is the bundle ID with ".mender"
// appended unless the ID already names a delta or full artifact (see
// stagedName).
type Staging struct {
	dir string
}

func NewStaging(dir string) *Staging {
	return &Staging{dir: dir}
}

func componentName(component byte) string {
	if component == ComponentDBC {
		return "dbc"
	}
	return "mdb"
}

func (st *Staging) componentDir(component byte) string {
	return filepath.Join(st.dir, componentName(component))
}

// stagedName returns the on-disk name of a completed bundle. IDs that already
// carry a known artifact extension are used verbatim — the phone sends the
// full asset filename for deltas so update-service can dispatch on the
// extension — while everything else is a full image and gets ".mender"
// appended (the phone historically sends the filename stem for those).
func stagedName(bundleID string) string {
	if strings.HasSuffix(bundleID, ".delta") || strings.HasSuffix(bundleID, ".mender") {
		return bundleID
	}
	return bundleID + ".mender"
}

// PartPath returns the path of the partial bundle file.
func (st *Staging) PartPath(component byte, bundleID string) string {
	return filepath.Join(st.componentDir(component), stagedName(bundleID)+".part")
}

// FinalPath returns the path the completed bundle is renamed to.
func (st *Staging) FinalPath(component byte, bundleID string) string {
	return filepath.Join(st.componentDir(component), stagedName(bundleID))
}

func (st *Staging) sidecarPath(component byte, bundleID string) string {
	return filepath.Join(st.componentDir(component), bundleID+".json")
}

// LoadSidecar returns the sidecar for a bundle, or nil when absent/corrupt.
func (st *Staging) LoadSidecar(component byte, bundleID string) *Sidecar {
	data, err := os.ReadFile(st.sidecarPath(component, bundleID))
	if err != nil {
		return nil
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil
	}
	return &sc
}

// WriteSidecar persists the sidecar durably (fsync file and directory).
func (st *Staging) WriteSidecar(component byte, sc *Sidecar) error {
	dir := st.componentDir(component)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	path := st.sidecarPath(component, sc.BundleID)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// Discard removes the partial file and sidecar of a bundle. Best-effort: a
// bundle only ever has some of these three paths present at once, so most
// calls are expected to remove files that were never there.
func (st *Staging) Discard(component byte, bundleID string) {
	_ = os.Remove(st.PartPath(component, bundleID))
	_ = os.Remove(st.FinalPath(component, bundleID))
	_ = os.Remove(st.sidecarPath(component, bundleID))
}

// Forget removes the transfer bookkeeping (partial file and sidecar) but keeps
// the completed bundle. Used after a successful MDB full-image install: the
// .mender stays in update-service's download dir as the base for future delta
// updates, and update-service's own retention policy owns it from here.
func (st *Staging) Forget(component byte, bundleID string) {
	_ = os.Remove(st.PartPath(component, bundleID))
	_ = os.Remove(st.sidecarPath(component, bundleID))
}

// PartSize returns the current size of the partial file (0 when absent).
func (st *Staging) PartSize(component byte, bundleID string) uint32 {
	info, err := os.Stat(st.PartPath(component, bundleID))
	if err != nil {
		return 0
	}
	if info.Size() < 0 {
		return 0
	}
	return uint32(info.Size())
}

// HasSpace reports whether the staging filesystem can hold `need` more bytes
// plus the safety margin.
func (st *Staging) HasSpace(need uint64) bool {
	if err := os.MkdirAll(st.dir, 0o755); err != nil {
		return false
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(st.dir, &fs); err != nil {
		// statfs failing is unusual; let the write path surface real errors
		return true
	}
	free := fs.Bavail * uint64(fs.Bsize)
	return free >= need+freeSpaceMargin
}

// CleanupStale removes partial transfers that have not been touched for a week
// and orphaned sidecars. Returns the number of files removed. Only .part files
// and their sidecars are touched: completed .mender files in these directories
// are owned by update-service (delta base retention), never by us.
func (st *Staging) CleanupStale() int {
	removed := 0
	for _, comp := range []string{"mdb", "dbc"} {
		dir := filepath.Join(st.dir, comp)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".mender.part") && !strings.HasSuffix(e.Name(), ".delta.part") {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleAge {
				continue
			}
			// invert stagedName: "id.mender.part" -> "id", "id.delta.part" -> "id.delta".
			// A bundle ID may also end in ".mender" verbatim, so try both sidecars.
			name := strings.TrimSuffix(e.Name(), ".part")
			_ = os.Remove(filepath.Join(dir, e.Name()))
			_ = os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".mender")+".json"))
			_ = os.Remove(filepath.Join(dir, name+".json"))
			removed++
		}
	}
	return removed
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dir, err)
	}
	return nil
}

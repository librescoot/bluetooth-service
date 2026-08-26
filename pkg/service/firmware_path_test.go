package service

import (
	"os"
	"path/filepath"
	"testing"
)

// A git describe version has its parts in the opposite order to a release tag,
// and the parser bins them by shape rather than position, so the canonical
// form reorders them. Lookup has to use the version as written or no untagged
// build can ever be found.
func TestFindFirmwarePathMatchesGitDescribeVersions(t *testing.T) {
	dir := t.TempDir()
	names := []string{
		"mdb-nrf52-app-v2.7.2-ls.zip",
		"mdb-nrf52-bl-v2.7.2-ls.zip",
		"mdb-nrf52-app-v2.7.2-ls-3-ge34b878.zip",
		"mdb-nrf52-bl-v2.7.2-ls-3-ge34b878.zip",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fu := &FirmwareUpdater{config: FirmwareUpdateConfig{FirmwareDir: dir}}

	for _, raw := range []string{"v2.7.2-ls-3-ge34b878", "v2.7.2-ls"} {
		v, err := ParseFirmwareVersion(raw)
		if err != nil {
			t.Fatalf("ParseFirmwareVersion(%q): %v", raw, err)
		}
		path, err := fu.findFirmwarePath(v)
		if err != nil {
			t.Errorf("findFirmwarePath(%q): %v (canonical form was %q)", raw, err, v.String())
			continue
		}
		if filepath.Base(path) == "" {
			t.Errorf("findFirmwarePath(%q) returned empty", raw)
		}
		t.Logf("%q -> %s", raw, filepath.Base(path))
	}
}

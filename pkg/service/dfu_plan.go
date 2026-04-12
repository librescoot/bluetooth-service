package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// dfuZipPattern matches DFU zip filenames of the form <prefix>-(bl|app)-<version>.zip.
// The prefix can be anything (the caller doesn't need to know what it is); pairing
// happens by requiring the prefix and version to match between the bl and app halves.
//
// Capture groups: [1]=prefix, [2]="bl"|"app", [3]=version string.
var dfuZipPattern = regexp.MustCompile(`^(.+)-(bl|app)-(v[^/]+)\.zip$`)

// ZipPair is the pair of DFU zips shipped alongside each nRF firmware
// release: the softdevice+bootloader package and the application package.
type ZipPair struct {
	Version string // release version string, e.g. "v2.1.0-ls"
	BLPath  string // absolute path to the *-bl-<version>.zip
	AppPath string // absolute path to the *-app-<version>.zip
}

// FindLatestZipPair scans dir for *-bl-<v>.zip and *-app-<v>.zip files that
// share the same prefix and version, and returns the pair belonging to the
// highest version where both halves are present. Returns (nil, nil) if no
// usable pair exists.
func FindLatestZipPair(dir string) (*ZipPair, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	// (prefix, version) -> {kind -> path}
	type pairKey struct{ prefix, version string }
	byPair := make(map[pairKey]map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := dfuZipPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		key := pairKey{prefix: m[1], version: m[3]}
		kind := m[2]
		if byPair[key] == nil {
			byPair[key] = make(map[string]string)
		}
		byPair[key][kind] = filepath.Join(dir, e.Name())
	}

	var best *ZipPair
	var bestParsed *FirmwareVersion
	for key, files := range byPair {
		bl, hasBL := files["bl"]
		app, hasApp := files["app"]
		if !hasBL || !hasApp {
			continue
		}
		parsed, err := ParseFirmwareVersion(key.version)
		if err != nil {
			continue
		}
		if bestParsed == nil || parsed.Compare(bestParsed) > 0 {
			best = &ZipPair{Version: key.version, BLPath: bl, AppPath: app}
			bestParsed = parsed
		}
	}
	return best, nil
}

// DFUStage is a single zip to flash as one Nordic DFU serial transaction.
type DFUStage struct {
	Label string // "bootloader+softdevice" or "application"
	Path  string // absolute path to the zip
}

// DFUPlan is the ordered list of stages that bring the nRF from its current
// state to the target release. An empty plan means the device is up to date.
type DFUPlan struct {
	Version string     // release the plan is upgrading to
	Stages  []DFUStage // run these in order, re-entering DFU between each
}

// BuildDFUPlan compares the installed versions against the target release's
// shipped zips and returns the minimal sequence of stages to close the gap.
//
//   - currentBLVersion is the bootloader_version integer reported by the nRF's
//     DFU serial GET_FIRMWARE_VERSION response (SLIP 0x0B 0x00).
//   - targetBLVersion comes from parsing the sd_bl.dat init packet in the
//     shipped -bl- zip.
//   - targetAppVersion comes from parsing the application .dat init packet in
//     the shipped -app- zip. It's only used here as a sanity check that the zip
//     isn't obviously corrupt; the decision to enter this code path at all is
//     gated upstream on the user-facing version string comparison.
//
// The bootloader stage is queued only when currentBL < targetBL. The app
// stage is always queued when we reach this function, because we only get
// here after the version-string check determined an update is needed.
func BuildDFUPlan(pair *ZipPair, currentBL, targetBL, targetAppVersion uint32) DFUPlan {
	plan := DFUPlan{Version: pair.Version}
	if currentBL < targetBL {
		plan.Stages = append(plan.Stages, DFUStage{
			Label: "bootloader+softdevice",
			Path:  pair.BLPath,
		})
	}
	// Target app version is available for logging but doesn't gate the stage.
	_ = targetAppVersion
	plan.Stages = append(plan.Stages, DFUStage{
		Label: "application",
		Path:  pair.AppPath,
	})
	return plan
}

// Summary returns a one-line human-readable description of the plan suitable
// for logging or the firmware-update-status redis hash.
func (p DFUPlan) Summary() string {
	if len(p.Stages) == 0 {
		return fmt.Sprintf("%s: up to date", p.Version)
	}
	labels := make([]string, 0, len(p.Stages))
	for _, s := range p.Stages {
		labels = append(labels, s.Label)
	}
	return fmt.Sprintf("%s: %s", p.Version, strings.Join(labels, " → "))
}

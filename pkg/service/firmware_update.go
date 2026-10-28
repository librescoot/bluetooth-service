package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// Bounds on the external DFU subprocesses. These are backstops against a hung
// tool, not expected durations: a real flash finishes in 1-3 min. Without them,
// a subprocess blocked on an unresponsive serial line never returns, and since
// RecoverFromDFU runs synchronously in Bootstrap() before the Redis command
// watcher starts, a bricked nRF at boot would wedge the whole service "active"
// but deaf. Kept generous so a slow-but-progressing flash is never killed
// mid-write (which could brick the nRF); the timeout only fires on a true hang.
const (
	dfuEnterTimeout = 3 * time.Minute
	dfuFlashTimeout = 10 * time.Minute
)

// powerInhibitsHash is the Redis hash pm-service watches for inhibitors that
// block suspend/hibernate. Shape is the same JSON that update-service writes
// (see pm-service/internal/inhibitor/redis.go).
const (
	powerInhibitsHash = "power:inhibits"
	dfuInhibitID      = "ble-dfu"

	// powerManagerHash / powerStateRunning: pm-service publishes its current
	// power state here. "running" is the only value that means no power
	// transition is pending or in flight; anything else (*-pending, *-imminent,
	// reboot, suspending, hibernating) means one is underway.
	powerManagerHash  = "power-manager"
	powerStateRunning = "running"
)

type powerInhibitData struct {
	ID       string `json:"id"`
	Who      string `json:"who"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Type     string `json:"type"`
	Duration int64  `json:"duration"`
	Created  int64  `json:"created"`
}

// addDFUInhibit registers a block-type power inhibitor for the duration of a
// firmware flash. Without it, pm-service has no way to know the UART is busy
// and the data stream has been stopped (which removes the implicit "UART
// frames keep us awake" guard added in d0daaa5), and could initiate suspend
// or hibernate mid-flash.
func (fu *FirmwareUpdater) addDFUInhibit(why string) {
	data := powerInhibitData{
		ID:      dfuInhibitID,
		Who:     "bluetooth-service",
		What:    "nrf52-firmware-update",
		Why:     why,
		Type:    "block",
		Created: time.Now().Unix(),
	}
	payload, err := json.Marshal(data)
	if err != nil {
		fu.log.Warnf("Failed to marshal DFU inhibitor: %v", err)
		return
	}
	if err := fu.ipc.Hash(powerInhibitsHash).Set(dfuInhibitID, string(payload)); err != nil {
		fu.log.Warnf("Failed to set DFU power inhibitor: %v", err)
		return
	}
	fu.log.Infof("Set power inhibitor %s (%s)", dfuInhibitID, why)
}

func (fu *FirmwareUpdater) removeDFUInhibit() {
	if err := fu.ipc.Hash(powerInhibitsHash).Delete(dfuInhibitID); err != nil {
		fu.log.Warnf("Failed to remove DFU power inhibitor: %v", err)
		return
	}
	fu.log.Infof("Cleared power inhibitor %s", dfuInhibitID)
}

// FirmwareUpdateConfig holds configuration for firmware updates
type FirmwareUpdateConfig struct {
	FirmwareDir      string
	NRFUpdateScript  string
	SerialDevice     string
	BaudRate         int
	MaxStageAttempts int
	DFUCooldown      int
}

// FirmwareUpdater handles nRF firmware updates
type FirmwareUpdater struct {
	config  FirmwareUpdateConfig
	log     *logger.Logger
	ipc     *ipc.Client
	service *Service

	// lastFlashedVersion is the zip version we last flashed. It guards against
	// reflashing the same version forever when the baked firmware reports a
	// version that never satisfies the filename version (dirty build / metadata
	// mismatch). Reset once the nRF reports up to date.
	lastFlashedVersion string

	// inProgress makes CheckAndUpdate single-flight: every VERSION response
	// spawns a check (boot, reconnects, link fallbacks), and re-inits DURING an
	// update produce version responses too — a concurrent second update would
	// fight over the serial port.
	inProgress atomic.Bool
}

// NewFirmwareUpdater creates a new FirmwareUpdater instance
func NewFirmwareUpdater(config FirmwareUpdateConfig, log *logger.Logger, ipcClient *ipc.Client, svc *Service) *FirmwareUpdater {
	// Set defaults
	if config.MaxStageAttempts == 0 {
		config.MaxStageAttempts = 3
	}
	if config.DFUCooldown == 0 {
		config.DFUCooldown = 5
	}
	if config.FirmwareDir == "" {
		config.FirmwareDir = DefaultFirmwareDir
	}
	if config.NRFUpdateScript == "" {
		config.NRFUpdateScript = DefaultNRFUpdateScript
	}
	if config.BaudRate == 0 {
		config.BaudRate = 115200
	}

	return &FirmwareUpdater{
		config:  config,
		log:     log,
		ipc:     ipcClient,
		service: svc,
	}
}

// setStatus updates the firmware-update-status field in the ble hash. Uses
// the synchronous Set option so back-to-back calls (e.g. "checking" then
// "idle" inside CheckAndUpdate) are seen by Redis in the order issued. The
// async default would let the second Exec race ahead of the first and leave
// the field stuck on the earlier value.
func (fu *FirmwareUpdater) setStatus(status string) {
	if err := fu.ipc.Hash("ble").Set("firmware-update-status", status, ipc.Sync()); err != nil {
		fu.log.Errorf("Failed to set firmware update status: %v", err)
	}
}

// FindAvailableFirmware finds all firmware zip files in the firmware directory
func (fu *FirmwareUpdater) FindAvailableFirmware() ([]*FirmwareVersion, error) {
	entries, err := os.ReadDir(fu.config.FirmwareDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // Directory doesn't exist, not an error
		}
		return nil, fmt.Errorf("failed to read firmware directory: %w", err)
	}

	var versions []*FirmwareVersion
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".zip") {
			continue
		}

		// Extract version from filename (e.g., "nrf-fw-v2.3.4-2-ls.zip")
		baseName := strings.TrimSuffix(name, ".zip")

		// Try to find version pattern in filename
		versionStr := extractVersionFromFilename(baseName)
		if versionStr == "" {
			fu.log.Debugf("Could not extract version from firmware file: %s", name)
			continue
		}

		version, err := ParseFirmwareVersion(versionStr)
		if err != nil {
			fu.log.Debugf("Could not parse version from firmware file %s: %v", name, err)
			continue
		}

		versions = append(versions, version)
	}

	// Sort versions in descending order (newest first)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Compare(versions[j]) > 0
	})

	return versions, nil
}

// extractVersionFromFilename extracts a version string from a firmware filename
func extractVersionFromFilename(filename string) string {
	// Look for patterns like "v1.2.3" or "v1.2.3-4" or "v1.2.3-4-ls"
	parts := strings.Split(filename, "-")
	for i, part := range parts {
		if strings.HasPrefix(part, "v") && len(part) > 1 {
			// Found potential version start, reconstruct version string
			versionParts := []string{part}
			for j := i + 1; j < len(parts); j++ {
				versionParts = append(versionParts, parts[j])
			}
			return strings.Join(versionParts, "-")
		}
	}
	return ""
}

// GetLatestFirmware returns the latest available firmware version and its path
func (fu *FirmwareUpdater) GetLatestFirmware() (*FirmwareVersion, string, error) {
	versions, err := fu.FindAvailableFirmware()
	if err != nil {
		return nil, "", err
	}
	if len(versions) == 0 {
		return nil, "", nil
	}

	latest := versions[0]

	// Find the firmware file path
	path, err := fu.findFirmwarePath(latest)
	if err != nil {
		return nil, "", err
	}

	return latest, path, nil
}

// findFirmwarePath finds the path to a firmware zip file for a given version
func (fu *FirmwareUpdater) findFirmwarePath(version *FirmwareVersion) (string, error) {
	entries, err := os.ReadDir(fu.config.FirmwareDir)
	if err != nil {
		return "", fmt.Errorf("failed to read firmware directory: %w", err)
	}

	// Match on the version as it was written, not the canonical form. String()
	// re-emits the parts in a fixed order, build then suffix, which round-trips
	// a release tag like v2.0.0-1-ls but reorders a git describe version like
	// v2.7.2-ls-3-ge34b878 into v2.7.2-3-ls-ge34b878. The parser bins the parts
	// by whether they look numeric rather than by position, so the original
	// order is not recoverable from the fields. Filenames carry the original,
	// so canonicalising here made every untagged build unfindable.
	versionStr := version.Raw
	if versionStr == "" {
		versionStr = version.String()
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".zip") {
			continue
		}

		// Check if this file matches the version
		if strings.Contains(name, versionStr) || strings.Contains(name, strings.TrimPrefix(versionStr, "v")) {
			return filepath.Join(fu.config.FirmwareDir, name), nil
		}
	}

	return "", fmt.Errorf("firmware file not found for version %s", versionStr)
}

// CheckAndUpdate compares the nRF's running application version against the
// latest -app- zip in the firmware directory and runs a staged update if they
// don't match. It's called from the USOCK version-string handler after each
// reconnect.
func (fu *FirmwareUpdater) CheckAndUpdate(currentVersionStr string) (bool, error) {
	if !fu.inProgress.CompareAndSwap(false, true) {
		fu.log.Infof("Firmware update check skipped: another update is in progress")
		return false, nil
	}
	defer fu.inProgress.Store(false)

	fu.setStatus("checking")

	currentVersion, err := ParseFirmwareVersion(currentVersionStr)
	if err != nil {
		fu.setStatus("idle")
		return false, fmt.Errorf("failed to parse current version: %w", err)
	}

	pair, err := FindLatestZipPair(fu.config.FirmwareDir)
	if err != nil {
		fu.setStatus("idle")
		return false, fmt.Errorf("scanning %s: %w", fu.config.FirmwareDir, err)
	}
	if pair == nil {
		fu.log.Infof("No firmware zip pair found in %s", fu.config.FirmwareDir)
		fu.setStatus("idle")
		return false, nil
	}

	latestVersion, err := ParseFirmwareVersion(pair.Version)
	if err != nil {
		fu.setStatus("idle")
		return false, fmt.Errorf("parsing zip pair version %q: %w", pair.Version, err)
	}

	if !latestVersion.IsNewerThan(currentVersion) {
		fu.log.Infof("Current firmware %s is up to date (latest: %s)", currentVersion, latestVersion)
		fu.lastFlashedVersion = ""
		fu.setStatus("idle")
		return false, nil
	}

	// Convergence guard: if we already flashed this exact zip version and the
	// nRF still reports something older, reflashing won't help — the filename
	// version and the baked firmware version disagree. Stop instead of looping.
	if fu.lastFlashedVersion == pair.Version {
		fu.log.Errorf("nRF still reports %s after flashing %s; not reflashing (zip filename vs firmware version mismatch — remove or rename the zip)", currentVersion, latestVersion)
		fu.setStatus("error")
		return false, nil
	}

	fu.log.Infof("Newer firmware available: %s (current: %s)", latestVersion, currentVersion)
	fu.lastFlashedVersion = pair.Version
	if err := fu.PerformStagedUpdate(pair); err != nil {
		return false, err
	}
	return true, nil
}

// PerformStagedUpdate runs the full two-stage flash flow against the given
// zip pair: enter DFU, query the installed bootloader version, skip the
// bootloader stage if it's already at least as new as the target, and flash
// the remaining stages in order re-entering DFU between each.
func (fu *FirmwareUpdater) PerformStagedUpdate(pair *ZipPair) error {
	return fu.runStagedUpdate(pair, "updating")
}

// RecoverFromDFU is the bricked-app recovery entry point. Finds the latest
// firmware zip pair on disk and runs a staged flash without going through
// the version-string compare path — the device is assumed to be sitting in
// DFU mode with no working application, so the BL version probed inside the
// staged flash is the only identity we have to drive the plan.
//
// Safe to call before USOCK has been opened: runStagedUpdate's "stop data
// stream / close USOCK / stop subs" prelude is a no-op when nothing is
// running, and the post-flash ReconnectUSock + SubscribeToRedisChannels
// brings the service up identically to a normal startup.
func (fu *FirmwareUpdater) RecoverFromDFU() error {
	pair, err := FindLatestZipPair(fu.config.FirmwareDir)
	if err != nil {
		fu.setStatus("idle")
		return fmt.Errorf("scanning %s: %w", fu.config.FirmwareDir, err)
	}
	if pair == nil {
		fu.setStatus("idle")
		return fmt.Errorf("no firmware zip pair found in %s", fu.config.FirmwareDir)
	}
	fu.log.Warnf("Recovering nRF stuck in DFU mode -> flashing %s", pair.Version)
	return fu.runStagedUpdate(pair, "recovering")
}

// runStagedUpdate is the shared implementation behind PerformStagedUpdate
// (runtime path, initialStatus="updating") and RecoverFromDFU (startup
// bricked-app path, initialStatus="recovering"). The two paths share every
// step; only the visible status string for the duration of the flash differs.
func (fu *FirmwareUpdater) runStagedUpdate(pair *ZipPair, initialStatus string) error {
	fu.log.Infof("Starting firmware update to %s (status=%s)", pair.Version, initialStatus)
	fu.setStatus(initialStatus)

	fu.addDFUInhibit(fmt.Sprintf("flashing nRF52 firmware to %s", pair.Version))
	defer fu.removeDFUInhibit()

	// Set-then-check: the inhibitor above blocks any power transition pm-service
	// evaluates from now on, but it can't recall one already committed. If a
	// reboot/suspend is already imminent or in flight, our inhibitor arrived too
	// late and flashing now risks power being cut mid-write (an interrupted
	// bootloader stage is an unrecoverable brick). Bail so the caller retries
	// once the transition settles; removeDFUInhibit runs via the defer above.
	// Reading pm's published state after setting the inhibitor narrows the race
	// to the redis round-trip rather than the whole multi-minute flash.
	if state, err := fu.ipc.Hash(powerManagerHash).Get("state"); err != nil {
		fu.log.Warnf("Could not read power-manager state before DFU (proceeding): %v", err)
	} else if state != "" && state != powerStateRunning {
		fu.setStatus("idle")
		return fmt.Errorf("aborting DFU: power transition already in progress (power-manager state=%q)", state)
	}

	// Parse target versions from the signed init packets inside the zips.
	blInfo, err := ParseZipDFUInfo(pair.BLPath)
	if err != nil {
		fu.setStatus("failed:manifest")
		return fmt.Errorf("parsing %s: %w", pair.BLPath, err)
	}
	blTarget, ok := blInfo.BootloaderInfo()
	if !ok {
		fu.setStatus("failed:manifest")
		return fmt.Errorf("no bootloader image in %s", pair.BLPath)
	}
	appInfo, err := ParseZipDFUInfo(pair.AppPath)
	if err != nil {
		fu.setStatus("failed:manifest")
		return fmt.Errorf("parsing %s: %w", pair.AppPath, err)
	}
	appTarget, ok := appInfo.ApplicationInfo()
	if !ok {
		fu.setStatus("failed:manifest")
		return fmt.Errorf("no application image in %s", pair.AppPath)
	}
	fu.log.Infof("Target: bootloader_version=%d application_version=%d",
		blTarget.FwVersion, appTarget.FwVersion)

	// Take exclusive ownership of the serial line: block the link manager's
	// negotiations/keepalive (it would otherwise reopen the port mid-flash and
	// steal DFU traffic) and wait out any in-flight negotiation, THEN settle
	// the wire at 115200 — the DFU bootloader and nrfupdate.py only talk
	// 115200. Ownership is returned via ReconnectUSock (Resume) on every exit
	// path of the update.
	fu.log.Infof("Suspending UART link management for DFU...")
	fu.service.Link().Suspend()
	if err := fu.service.EnsureLinkBaud(115200); err != nil {
		fu.log.Warnf("Failed to downshift link baud before DFU (continuing anyway): %v", err)
	}

	// Tell the nRF to stop its autonomous data stream before we hand the
	// serial line to nrfupdate.py. Otherwise the data stream frames arrive
	// faster than the 1s read timeout in nrfupdate.py's serial_request(),
	// and the DFU entry stalls for minutes while bytes keep dripping in.
	fu.log.Infof("Stopping nRF data stream...")
	if err := writeUARTMessage(fu.service.GetUSock(), ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		fu.log.Warnf("Failed to stop data stream (continuing anyway): %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	fu.log.Infof("Stopping Redis subscriptions...")
	fu.service.StopSubscriptions()
	fu.log.Infof("Closing serial connection...")
	if err := fu.service.CloseUSock(); err != nil {
		fu.log.Errorf("Failed to close serial connection: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	// Enter DFU mode for the first time. All plan decisions and flashes
	// happen while the nRF is in DFU bootloader mode.
	fu.log.Infof("Entering DFU mode...")
	if err := fu.enterDFUMode(); err != nil {
		fu.setStatus("failed:nrfupdate")
		fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("DFU mode entry failed: %v", err))
		fu.attemptReconnect()
		return fmt.Errorf("failed to enter DFU mode: %w", err)
	}

	// Query the currently installed bootloader version. If the query fails,
	// fall back to "flash the bootloader anyway" — the nordicsemi tool will
	// still soft-fail on 0x05 if the bootloader is already current, leaving
	// the app flash to proceed as its own transaction.
	var currentBL uint32
	if info, err := QueryDFUFirmwareVersion(fu.config.SerialDevice, fu.config.BaudRate, nrfDFUImageIdxBootloader, 2*time.Second); err != nil {
		fu.log.Warnf("Unable to query installed bootloader version: %v. Will attempt BL flash anyway.", err)
		currentBL = 0
	} else {
		currentBL = info.Version
		fu.log.Infof("Installed bootloader_version=%d (image_type=0x%02x, addr=0x%08x, len=%d)",
			info.Version, info.ImageType, info.Addr, info.Length)
	}

	plan := BuildDFUPlan(pair, currentBL, blTarget.FwVersion, appTarget.FwVersion)
	fu.log.Infof("DFU plan: %s", plan.Summary())

	// Execute each stage. Between stages the nRF reboots out of DFU mode
	// back into the bootloader, so we re-enter DFU before each subsequent
	// flash. Each stage is retried up to MaxStageAttempts times — re-entering
	// DFU before every retry, since a failed flash may have left the BL in an
	// unknown state. Stage 1 attempt 1 skips re-entry because DFU mode was
	// already entered at the top of this function.
	for i, stage := range plan.Stages {
		if err := fu.runStageWithRetry(stage, i, len(plan.Stages)); err != nil {
			fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("DFU stage %d/%d (%s) failed: %v", i+1, len(plan.Stages), stage.Label, err))
			fu.attemptReconnect()
			return err
		}
	}

	// Wait for the nRF to reboot into the freshly-flashed application and
	// reopen USOCK.
	fu.log.Infof("Waiting for nRF to reboot...")
	time.Sleep(20 * time.Second)

	fu.log.Infof("Reconnecting to nRF...")
	if err := fu.service.ReconnectUSock(); err != nil {
		fu.setStatus("failed:reconnect")
		fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("reconnect after update failed: %v", err))
		return fmt.Errorf("failed to reconnect after update: %w", err)
	}

	fu.log.Infof("Restarting Redis subscriptions...")
	fu.service.SubscribeToRedisChannels()

	fu.service.ClearFault(FaultFirmwareUpdate)
	fu.setStatus("success")
	fu.log.Infof("Firmware update to %s completed successfully", pair.Version)
	return nil
}

// runStageWithRetry flashes one DFU stage, retrying on transient UART or
// nordicsemi failures up to MaxStageAttempts times. It re-enters DFU mode
// before every retry because a failed flash may have left the bootloader in
// an unknown state — sending the DFU command again is benign whether the BL
// is alive (it accepts the request) or the device just rebooted (the new BL
// session catches the next request).
//
// On exhausted retries, sets the firmware-update-status hash to either
// failed:nrfupdate (couldn't enter DFU) or failed:dfu (entered DFU but the
// flash itself failed), preserving the distinction the single-attempt path
// used to make.
func (fu *FirmwareUpdater) runStageWithRetry(stage DFUStage, stageIdx, stageCount int) error {
	maxAttempts := max(fu.config.MaxStageAttempts, 1)

	type errPhase int
	const (
		phaseEnter errPhase = iota
		phaseFlash
	)

	var lastErr error
	var lastPhase errPhase

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Stage 1 attempt 1 inherits the DFU entry done by the caller; every
		// other (stage, attempt) pair re-enters DFU first.
		if !(stageIdx == 0 && attempt == 1) {
			if attempt > 1 {
				fu.log.Infof("Re-entering DFU mode for stage %d (%s), retry %d/%d...",
					stageIdx+1, stage.Label, attempt, maxAttempts)
			} else {
				fu.log.Infof("Re-entering DFU mode for stage %d (%s)...", stageIdx+1, stage.Label)
			}
			if err := fu.enterDFUMode(); err != nil {
				lastErr = fmt.Errorf("entering DFU mode for stage %s: %w", stage.Label, err)
				lastPhase = phaseEnter
				fu.log.Warnf("DFU entry failed (stage %s, attempt %d/%d): %v",
					stage.Label, attempt, maxAttempts, err)
				continue
			}
		}

		fu.log.Infof("Flashing stage %d/%d: %s (%s) [attempt %d/%d]",
			stageIdx+1, stageCount, stage.Label, stage.Path, attempt, maxAttempts)
		if err := fu.runNordicDFU(stage.Path); err != nil {
			lastErr = fmt.Errorf("DFU failed on stage %s: %w", stage.Label, err)
			lastPhase = phaseFlash
			fu.log.Warnf("DFU flash failed (stage %s, attempt %d/%d): %v",
				stage.Label, attempt, maxAttempts, err)
			continue
		}

		return nil
	}

	switch lastPhase {
	case phaseEnter:
		fu.setStatus("failed:nrfupdate")
	default:
		fu.setStatus("failed:dfu")
	}
	return fmt.Errorf("stage %s exhausted %d attempts: %w", stage.Label, maxAttempts, lastErr)
}

// PerformUpdate is the legacy single-zip entry point. New code should call
// PerformStagedUpdate with a ZipPair so the bootloader-vs-application split
// logic runs. This shim exists so the redis force-update command and any
// out-of-tree callers keep working until they're migrated.
func (fu *FirmwareUpdater) PerformUpdate(firmwarePath string) error {
	// If the caller handed us one of the new split zips, promote it to a
	// pair and run the staged flow.
	base := filepath.Base(firmwarePath)
	if m := dfuZipPattern.FindStringSubmatch(base); m != nil {
		prefix, version := m[1], m[3]
		dir := filepath.Dir(firmwarePath)
		pair := &ZipPair{
			Version: version,
			BLPath:  filepath.Join(dir, prefix+"-bl-"+version+".zip"),
			AppPath: filepath.Join(dir, prefix+"-app-"+version+".zip"),
		}
		if fileExists(pair.BLPath) && fileExists(pair.AppPath) {
			return fu.PerformStagedUpdate(pair)
		}
		fu.log.Warnf("PerformUpdate: %s missing its sibling, falling back to single-zip flash", firmwarePath)
	}

	// Fallback: the caller passed a zip we can't interpret as a pair (a
	// legacy -full- zip, for example). Flash it as one shot without version
	// decisions. This path is only used for backward compatibility and will
	// be removed once all callers use staged updates.
	return fu.legacyPerformUpdate(firmwarePath)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// legacyPerformUpdate is the pre-split flash path: stop subscriptions, enter
// DFU, flash one zip, reconnect. Used only for force-update commands that
// haven't been migrated to the staged flow.
func (fu *FirmwareUpdater) legacyPerformUpdate(firmwarePath string) error {
	fu.log.Infof("Starting firmware update with %s", firmwarePath)
	fu.setStatus("updating")

	fu.addDFUInhibit(fmt.Sprintf("flashing nRF52 firmware from %s", filepath.Base(firmwarePath)))
	defer fu.removeDFUInhibit()

	// take exclusive port ownership from the link manager, then settle the
	// wire at 115200 (DFU bootloader talks 115200 only); ownership is returned
	// via ReconnectUSock (Resume) on every exit path
	fu.service.Link().Suspend()
	if err := fu.service.EnsureLinkBaud(115200); err != nil {
		fu.log.Warnf("Failed to downshift link baud before DFU (continuing anyway): %v", err)
	}

	fu.log.Infof("Stopping nRF data stream...")
	if err := writeUARTMessage(fu.service.GetUSock(), ble.TypeDataStream, ble.TypeDataStreamEnable, 0); err != nil {
		fu.log.Warnf("Failed to stop data stream (continuing anyway): %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	fu.log.Infof("Stopping Redis subscriptions...")
	fu.service.StopSubscriptions()

	fu.log.Infof("Closing serial connection...")
	if err := fu.service.CloseUSock(); err != nil {
		fu.log.Errorf("Failed to close serial connection: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	fu.log.Infof("Entering DFU mode...")
	if err := fu.enterDFUMode(); err != nil {
		fu.setStatus("failed:nrfupdate")
		fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("DFU mode entry failed: %v", err))
		fu.attemptReconnect()
		return fmt.Errorf("failed to enter DFU mode: %w", err)
	}

	fu.log.Infof("Running Nordic DFU...")
	if err := fu.runNordicDFU(firmwarePath); err != nil {
		fu.setStatus("failed:dfu")
		fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("Nordic DFU failed: %v", err))
		fu.attemptReconnect()
		return fmt.Errorf("DFU failed: %w", err)
	}

	fu.log.Infof("Waiting for nRF to reboot...")
	time.Sleep(20 * time.Second)

	fu.log.Infof("Reconnecting to nRF...")
	if err := fu.service.ReconnectUSock(); err != nil {
		fu.setStatus("failed:reconnect")
		fu.service.SetFault(FaultFirmwareUpdate, fmt.Sprintf("reconnect after update failed: %v", err))
		return fmt.Errorf("failed to reconnect after update: %w", err)
	}

	fu.log.Infof("Restarting Redis subscriptions...")
	fu.service.SubscribeToRedisChannels()

	fu.service.ClearFault(FaultFirmwareUpdate)
	fu.setStatus("success")
	fu.log.Infof("Firmware update completed successfully")
	return nil
}

// enterDFUMode runs nrfupdate.py to put the nRF into DFU mode.
//
// The script sends a UART break to wake a potentially-hung nRF, then transmits
// the USOCK DFU_CMD_START command. The nRF reboots into the DFU bootloader
// without sending a reply, so we treat a clean subprocess exit as success.
// The downstream SLIP version query or nordicsemi ping is the real verification
// that the bootloader is alive.
func (fu *FirmwareUpdater) enterDFUMode() error {
	ctx, cancel := context.WithTimeout(context.Background(), dfuEnterTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", fu.config.NRFUpdateScript, "-p", fu.config.SerialDevice, "--dfu", "usock")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	err := cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fu.log.Errorf("nrfupdate.py timed out after %v (serial line unresponsive?), output: %s", dfuEnterTimeout, output.String())
			return fmt.Errorf("nrfupdate.py timed out after %v: %w", dfuEnterTimeout, ctx.Err())
		}
		fu.log.Errorf("nrfupdate.py failed: %v, output: %s", err, output.String())
		return fmt.Errorf("nrfupdate.py: %w", err)
	}

	fu.log.Infof("DFU entry command sent (%s)", strings.TrimSpace(output.String()))
	return nil
}

// runNordicDFU runs the Nordic DFU tool
func (fu *FirmwareUpdater) runNordicDFU(firmwarePath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dfuFlashTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", "-m", "nordicsemi", "dfu", "serial",
		"-p", fu.config.SerialDevice,
		"-pkg", firmwarePath,
		"-cd", fmt.Sprintf("%d", fu.config.DFUCooldown))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	fu.log.Debugf("Running: python3 -m nordicsemi dfu serial -p %s -pkg %s -cd %d",
		fu.config.SerialDevice, firmwarePath, fu.config.DFUCooldown)

	err := cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			fu.log.Errorf("Nordic DFU timed out after %v (serial line unresponsive?)", dfuFlashTimeout)
			fu.log.Errorf("stdout: %s", stdout.String())
			fu.log.Errorf("stderr: %s", stderr.String())
			return fmt.Errorf("nordic DFU timed out after %v: %w", dfuFlashTimeout, ctx.Err())
		}
		fu.log.Errorf("Nordic DFU failed: %v", err)
		fu.log.Errorf("stdout: %s", stdout.String())
		fu.log.Errorf("stderr: %s", stderr.String())
		return fmt.Errorf("nordic DFU failed: %w", err)
	}

	fu.log.Infof("Nordic DFU completed successfully")
	return nil
}

// attemptReconnect tries to reconnect after a failed update
func (fu *FirmwareUpdater) attemptReconnect() {
	fu.log.Infof("Attempting to reconnect after failed update...")
	time.Sleep(2 * time.Second)
	if err := fu.service.ReconnectUSock(); err != nil {
		fu.log.Errorf("Failed to reconnect after failed update: %v", err)
	} else {
		fu.log.Infof("Reconnected successfully after failed update")
		// Restart subscriptions to sync state
		fu.service.SubscribeToRedisChannels()
	}
}

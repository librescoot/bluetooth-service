package service

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// FirmwareUpdateConfig holds configuration for firmware updates
type FirmwareUpdateConfig struct {
	FirmwareDir     string
	NRFUpdateScript string
	SerialDevice    string
	MaxDFURetries   int
	DFURetryDelay   time.Duration
	DFUCooldown     int
}

// FirmwareUpdater handles nRF firmware updates
type FirmwareUpdater struct {
	config  FirmwareUpdateConfig
	log     *logger.Logger
	ipc     *ipc.Client
	service *Service
}

// NewFirmwareUpdater creates a new FirmwareUpdater instance
func NewFirmwareUpdater(config FirmwareUpdateConfig, log *logger.Logger, ipcClient *ipc.Client, svc *Service) *FirmwareUpdater {
	// Set defaults
	if config.MaxDFURetries == 0 {
		config.MaxDFURetries = 60
	}
	if config.DFURetryDelay == 0 {
		config.DFURetryDelay = time.Second
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

	return &FirmwareUpdater{
		config:  config,
		log:     log,
		ipc:     ipcClient,
		service: svc,
	}
}

// setStatus updates the firmware update status in Redis
func (fu *FirmwareUpdater) setStatus(status string) {
	if err := fu.ipc.Hash("ble").Set("firmware-update-status", status); err != nil {
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

	versionStr := version.String()
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

// CheckAndUpdate checks for newer firmware and updates if available
func (fu *FirmwareUpdater) CheckAndUpdate(currentVersionStr string) (bool, error) {
	fu.setStatus("checking")

	currentVersion, err := ParseFirmwareVersion(currentVersionStr)
	if err != nil {
		fu.setStatus("idle")
		return false, fmt.Errorf("failed to parse current version: %w", err)
	}

	latestVersion, firmwarePath, err := fu.GetLatestFirmware()
	if err != nil {
		fu.setStatus("idle")
		return false, fmt.Errorf("failed to get latest firmware: %w", err)
	}

	if latestVersion == nil {
		fu.log.Infof("No firmware files found in %s", fu.config.FirmwareDir)
		fu.setStatus("idle")
		return false, nil
	}

	if !latestVersion.IsNewerThan(currentVersion) {
		fu.log.Infof("Current firmware %s is up to date (latest: %s)", currentVersion, latestVersion)
		fu.setStatus("idle")
		return false, nil
	}

	if latestVersion.Compare(currentVersion) > 0 {
		fu.log.Infof("Newer firmware available: %s (current: %s)", latestVersion, currentVersion)
	} else {
		fu.log.Infof("Firmware build metadata changed: %s (current: %s)", latestVersion, currentVersion)
	}

	// Perform the update
	if err := fu.PerformUpdate(firmwarePath); err != nil {
		return false, err
	}

	return true, nil
}

// PerformUpdate performs the firmware update process
func (fu *FirmwareUpdater) PerformUpdate(firmwarePath string) error {
	fu.log.Infof("Starting firmware update with %s", firmwarePath)
	fu.setStatus("updating")

	// Step 1: Stop subscriptions (they will be restarted after reconnect)
	fu.log.Infof("Stopping Redis subscriptions...")
	fu.service.StopSubscriptions()

	// Step 2: Close serial connection
	fu.log.Infof("Closing serial connection...")
	if err := fu.service.CloseUSock(); err != nil {
		fu.log.Errorf("Failed to close serial connection: %v", err)
		// Continue anyway, the update might still work
	}

	// Step 3: Wait for port release
	time.Sleep(500 * time.Millisecond)

	// Step 4: Run nrfupdate.py to enter DFU mode
	fu.log.Infof("Entering DFU mode...")
	if err := fu.enterDFUMode(); err != nil {
		fu.setStatus("failed:nrfupdate")
		fu.service.SetFault(FaultFirmwareUpdate)
		fu.attemptReconnect()
		return fmt.Errorf("failed to enter DFU mode: %w", err)
	}

	// Step 5: Run Nordic DFU tool
	fu.log.Infof("Running Nordic DFU...")
	if err := fu.runNordicDFU(firmwarePath); err != nil {
		fu.setStatus("failed:dfu")
		fu.service.SetFault(FaultFirmwareUpdate)
		fu.attemptReconnect()
		return fmt.Errorf("DFU failed: %w", err)
	}

	// Step 6: Wait for nRF to reboot
	fu.log.Infof("Waiting for nRF to reboot...")
	time.Sleep(20 * time.Second)

	// Step 7: Reconnect
	fu.log.Infof("Reconnecting to nRF...")
	if err := fu.service.ReconnectUSock(); err != nil {
		fu.setStatus("failed:reconnect")
		fu.service.SetFault(FaultFirmwareUpdate)
		return fmt.Errorf("failed to reconnect after update: %w", err)
	}

	// Step 8: Restart subscriptions (this will sync state via StartWithSync)
	fu.log.Infof("Restarting Redis subscriptions...")
	fu.service.SubscribeToRedisChannels()

	fu.service.ClearFault(FaultFirmwareUpdate)
	fu.setStatus("success")
	fu.log.Infof("Firmware update completed successfully")
	return nil
}

// enterDFUMode runs nrfupdate.py to put the nRF into DFU mode
func (fu *FirmwareUpdater) enterDFUMode() error {
	for attempt := 1; attempt <= fu.config.MaxDFURetries; attempt++ {
		fu.log.Debugf("DFU mode attempt %d/%d", attempt, fu.config.MaxDFURetries)

		cmd := exec.Command("python3", fu.config.NRFUpdateScript, "-p", fu.config.SerialDevice, "--dfu", "usock")
		var output bytes.Buffer
		// Capture both stdout and stderr since Python scripts may output to either
		cmd.Stdout = &output
		cmd.Stderr = &output

		err := cmd.Run()
		outputStr := output.String()

		if err != nil {
			fu.log.Debugf("nrfupdate.py attempt %d failed: %v, output: %s", attempt, err, outputStr)
			time.Sleep(fu.config.DFURetryDelay)
			continue
		}

		fu.log.Debugf("nrfupdate.py attempt %d output: %s", attempt, outputStr)

		// Check if we got a response after "Rx:"
		if hasValidDFUResponse(outputStr) {
			fu.log.Infof("DFU mode entered successfully")
			return nil
		}

		fu.log.Debugf("No valid DFU response on attempt %d", attempt)
		time.Sleep(fu.config.DFURetryDelay)
	}

	return fmt.Errorf("failed to enter DFU mode after %d attempts", fu.config.MaxDFURetries)
}

// hasValidDFUResponse checks if the nrfupdate.py output contains a valid DFU response
func hasValidDFUResponse(output string) bool {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Rx:") {
			// Check if there's content after "Rx:"
			rxContent := strings.TrimPrefix(line, "Rx:")
			rxContent = strings.TrimSpace(rxContent)
			if rxContent != "" {
				return true
			}
		}
	}
	return false
}

// runNordicDFU runs the Nordic DFU tool
func (fu *FirmwareUpdater) runNordicDFU(firmwarePath string) error {
	cmd := exec.Command("python3", "-m", "nordicsemi", "dfu", "serial",
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

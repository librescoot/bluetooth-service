package service

import (
	"fmt"
	"strconv"
	"strings"
)

// Minimum supported firmware version
const (
	MinMajor = 1
	MinMinor = 12
	MinPatch = 0
)

// FirmwareVersion represents a semantic version number
type FirmwareVersion struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

// ParseFirmwareVersion parses a version string like "v1.12.0" or "1.12.0"
func ParseFirmwareVersion(versionStr string) (*FirmwareVersion, error) {
	if versionStr == "" {
		return nil, fmt.Errorf("version string is empty")
	}

	// Store raw version for logging
	raw := versionStr

	// Remove 'v' prefix if present
	versionStr = strings.TrimPrefix(versionStr, "v")

	// Split into components
	parts := strings.Split(versionStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid version format: expected major.minor.patch, got %s", raw)
	}

	// Parse major version
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid major version: %v", err)
	}

	// Parse minor version
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid minor version: %v", err)
	}

	// Parse patch version
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("invalid patch version: %v", err)
	}

	return &FirmwareVersion{
		Major: major,
		Minor: minor,
		Patch: patch,
		Raw:   raw,
	}, nil
}

// IsCompatible checks if the firmware version meets minimum requirements
func (v *FirmwareVersion) IsCompatible() bool {
	// Check major version
	if v.Major > MinMajor {
		return true
	}
	if v.Major < MinMajor {
		return false
	}

	// Major version equals minimum, check minor version
	if v.Minor > MinMinor {
		return true
	}
	if v.Minor < MinMinor {
		return false
	}

	// Major and minor equal minimum, check patch version
	return v.Patch >= MinPatch
}

// String returns a string representation of the version
func (v *FirmwareVersion) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

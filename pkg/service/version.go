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

// FirmwareVersion represents a semantic version number with optional build and suffix
type FirmwareVersion struct {
	Major  int
	Minor  int
	Patch  int
	Build  int    // Optional build number (e.g., -1 in v2.0.0-1-ls)
	Suffix string // Optional suffix (e.g., "ls" in v2.0.0-1-ls)
	Raw    string
}

// ParseFirmwareVersion parses a version string like "v1.12.0", "1.12.0", or extended
// formats like "v2.0.0-1-ls" (with optional build number and suffix)
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

	// Parse patch and optional build/suffix (e.g., "0-1-ls" or "0-ls" or "0-1" or "0")
	patchParts := strings.Split(parts[2], "-")

	// Parse patch version (first part is always the patch number)
	patch, err := strconv.Atoi(patchParts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid patch version: %v", err)
	}

	var build int
	var suffix string

	// Parse optional build number and suffix
	for i := 1; i < len(patchParts); i++ {
		part := patchParts[i]
		if part == "" {
			continue
		}

		// Try to parse as build number (numeric)
		if num, err := strconv.Atoi(part); err == nil && build == 0 {
			build = num
		} else {
			// Non-numeric part is the suffix (append if multiple)
			if suffix == "" {
				suffix = part
			} else {
				suffix = suffix + "-" + part
			}
		}
	}

	return &FirmwareVersion{
		Major:  major,
		Minor:  minor,
		Patch:  patch,
		Build:  build,
		Suffix: suffix,
		Raw:    raw,
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
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Build > 0 {
		s += fmt.Sprintf("-%d", v.Build)
	}
	if v.Suffix != "" {
		s += "-" + v.Suffix
	}
	return s
}

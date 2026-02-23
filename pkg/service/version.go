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
	Major         int
	Minor         int
	Patch         int
	Build         int    // Optional build number (e.g., -1 in v2.0.0-1-ls)
	Suffix        string // Optional suffix (e.g., "ls" in v2.0.0-1-ls)
	BuildMetadata string // Semver build metadata after "+" (e.g., "alarm" in v1.12.0+alarm); ignored in comparisons
	Raw           string
}

// ParseFirmwareVersion parses a version string like "v1.12.0", "1.12.0", extended
// formats like "v2.0.0-1-ls" (with optional build number and suffix), or semver
// build metadata format like "v1.12.0+alarm". Build metadata after "+" is stored
// but ignored in version comparisons per the semver spec.
func ParseFirmwareVersion(versionStr string) (*FirmwareVersion, error) {
	if versionStr == "" {
		return nil, fmt.Errorf("version string is empty")
	}

	// Store raw version for logging
	raw := versionStr

	// Remove 'v' prefix if present
	versionStr = strings.TrimPrefix(versionStr, "v")

	// Strip semver build metadata ("+..."); per semver spec it is ignored in comparisons
	var buildMetadata string
	if idx := strings.IndexByte(versionStr, '+'); idx >= 0 {
		buildMetadata = versionStr[idx+1:]
		versionStr = versionStr[:idx]
	}

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
		Major:         major,
		Minor:         minor,
		Patch:         patch,
		Build:         build,
		Suffix:        suffix,
		BuildMetadata: buildMetadata,
		Raw:           raw,
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

// String returns a string representation of the version.
// Build metadata (if present) is appended with "+" per semver spec.
func (v *FirmwareVersion) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Build > 0 {
		s += fmt.Sprintf("-%d", v.Build)
	}
	if v.Suffix != "" {
		s += "-" + v.Suffix
	}
	if v.BuildMetadata != "" {
		s += "+" + v.BuildMetadata
	}
	return s
}

// IsNewerThan returns true if this version should replace the installed version.
// A higher base version always triggers an update. When the base version is equal,
// an update is also triggered if the build metadata differs — e.g. the device
// reports v1.12.0 but the available firmware is v1.12.0+alarm.
func (v *FirmwareVersion) IsNewerThan(installed *FirmwareVersion) bool {
	cmp := v.Compare(installed)
	if cmp > 0 {
		return true
	}
	if cmp == 0 {
		return v.BuildMetadata != installed.BuildMetadata
	}
	return false
}

// Compare compares this version with another version.
// Returns -1 if v < other, 0 if v == other, 1 if v > other.
// Comparison order: Major > Minor > Patch > Build. Suffix and BuildMetadata are ignored.
func (v *FirmwareVersion) Compare(other *FirmwareVersion) int {
	if other == nil {
		return 1
	}

	// Compare major version
	if v.Major < other.Major {
		return -1
	}
	if v.Major > other.Major {
		return 1
	}

	// Compare minor version
	if v.Minor < other.Minor {
		return -1
	}
	if v.Minor > other.Minor {
		return 1
	}

	// Compare patch version
	if v.Patch < other.Patch {
		return -1
	}
	if v.Patch > other.Patch {
		return 1
	}

	// Compare build number
	if v.Build < other.Build {
		return -1
	}
	if v.Build > other.Build {
		return 1
	}

	return 0
}

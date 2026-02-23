package service

import (
	"testing"
)

func TestParseFirmwareVersion(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantMajor     int
		wantMinor     int
		wantPatch     int
		wantBuild     int
		wantSuffix    string
		wantBuildMeta string
		wantErr       bool
		errContains   string
	}{
		{
			name:      "valid version with v prefix",
			input:     "v1.12.0",
			wantMajor: 1,
			wantMinor: 12,
			wantPatch: 0,
			wantErr:   false,
		},
		{
			name:      "valid version without v prefix",
			input:     "1.12.0",
			wantMajor: 1,
			wantMinor: 12,
			wantPatch: 0,
			wantErr:   false,
		},
		{
			name:      "higher version",
			input:     "v2.0.1",
			wantMajor: 2,
			wantMinor: 0,
			wantPatch: 1,
			wantErr:   false,
		},
		{
			name:      "patch version",
			input:     "v1.12.5",
			wantMajor: 1,
			wantMinor: 12,
			wantPatch: 5,
			wantErr:   false,
		},
		{
			name:       "version with build number",
			input:      "v2.0.0-1",
			wantMajor:  2,
			wantMinor:  0,
			wantPatch:  0,
			wantBuild:  1,
			wantSuffix: "",
			wantErr:    false,
		},
		{
			name:       "version with suffix only",
			input:      "v2.0.0-ls",
			wantMajor:  2,
			wantMinor:  0,
			wantPatch:  0,
			wantBuild:  0,
			wantSuffix: "ls",
			wantErr:    false,
		},
		{
			name:       "version with build and suffix",
			input:      "v2.0.0-1-ls",
			wantMajor:  2,
			wantMinor:  0,
			wantPatch:  0,
			wantBuild:  1,
			wantSuffix: "ls",
			wantErr:    false,
		},
		{
			name:       "version with higher build number",
			input:      "v1.12.3-42-ls",
			wantMajor:  1,
			wantMinor:  12,
			wantPatch:  3,
			wantBuild:  42,
			wantSuffix: "ls",
			wantErr:    false,
		},
		{
			name:       "version with multi-part suffix",
			input:      "v2.0.0-1-ls-beta",
			wantMajor:  2,
			wantMinor:  0,
			wantPatch:  0,
			wantBuild:  1,
			wantSuffix: "ls-beta",
			wantErr:    false,
		},
		{
			name:        "empty string",
			input:       "",
			wantErr:     true,
			errContains: "empty",
		},
		{
			name:        "invalid format - missing parts",
			input:       "v1.12",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:        "invalid format - too many parts",
			input:       "v1.12.0.1",
			wantErr:     true,
			errContains: "invalid version format",
		},
		{
			name:          "semver build metadata only",
			input:         "v1.12.0+alarm",
			wantMajor:     1,
			wantMinor:     12,
			wantPatch:     0,
			wantBuildMeta: "alarm",
			wantErr:       false,
		},
		{
			name:          "semver build metadata without v prefix",
			input:         "1.12.0+alarm",
			wantMajor:     1,
			wantMinor:     12,
			wantPatch:     0,
			wantBuildMeta: "alarm",
			wantErr:       false,
		},
		{
			name:          "semver build metadata with pre-release suffix",
			input:         "v2.0.0-1-ls+build42",
			wantMajor:     2,
			wantMinor:     0,
			wantPatch:     0,
			wantBuild:     1,
			wantSuffix:    "ls",
			wantBuildMeta: "build42",
			wantErr:       false,
		},
		{
			name:          "semver build metadata with dot-separated identifiers",
			input:         "v1.12.0+exp.sha.5114f85",
			wantMajor:     1,
			wantMinor:     12,
			wantPatch:     0,
			wantBuildMeta: "exp.sha.5114f85",
			wantErr:       false,
		},
		{
			name:        "invalid major version",
			input:       "vX.12.0",
			wantErr:     true,
			errContains: "invalid major version",
		},
		{
			name:        "invalid minor version",
			input:       "v1.X.0",
			wantErr:     true,
			errContains: "invalid minor version",
		},
		{
			name:        "invalid patch version",
			input:       "v1.12.X",
			wantErr:     true,
			errContains: "invalid patch version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFirmwareVersion(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Errorf("ParseFirmwareVersion() expected error but got none")
					return
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("ParseFirmwareVersion() error = %v, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Errorf("ParseFirmwareVersion() unexpected error: %v", err)
				return
			}

			if got.Major != tt.wantMajor {
				t.Errorf("ParseFirmwareVersion() Major = %v, want %v", got.Major, tt.wantMajor)
			}
			if got.Minor != tt.wantMinor {
				t.Errorf("ParseFirmwareVersion() Minor = %v, want %v", got.Minor, tt.wantMinor)
			}
			if got.Patch != tt.wantPatch {
				t.Errorf("ParseFirmwareVersion() Patch = %v, want %v", got.Patch, tt.wantPatch)
			}
			if got.Build != tt.wantBuild {
				t.Errorf("ParseFirmwareVersion() Build = %v, want %v", got.Build, tt.wantBuild)
			}
			if got.Suffix != tt.wantSuffix {
				t.Errorf("ParseFirmwareVersion() Suffix = %v, want %v", got.Suffix, tt.wantSuffix)
			}
			if got.BuildMetadata != tt.wantBuildMeta {
				t.Errorf("ParseFirmwareVersion() BuildMetadata = %v, want %v", got.BuildMetadata, tt.wantBuildMeta)
			}
			if got.Raw != tt.input {
				t.Errorf("ParseFirmwareVersion() Raw = %v, want %v", got.Raw, tt.input)
			}
		})
	}
}

func TestFirmwareVersion_IsCompatible(t *testing.T) {
	tests := []struct {
		name    string
		version FirmwareVersion
		want    bool
	}{
		{
			name:    "exact minimum version",
			version: FirmwareVersion{Major: MinMajor, Minor: MinMinor, Patch: MinPatch},
			want:    true,
		},
		{
			name:    "higher patch version",
			version: FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			want:    true,
		},
		{
			name:    "higher minor version",
			version: FirmwareVersion{Major: 1, Minor: 13, Patch: 0},
			want:    true,
		},
		{
			name:    "higher major version",
			version: FirmwareVersion{Major: 2, Minor: 0, Patch: 0},
			want:    true,
		},
		{
			name:    "lower patch version",
			version: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:    true, // 1.12.0 is minimum, so this is compatible
		},
		{
			name:    "lower minor version",
			version: FirmwareVersion{Major: 1, Minor: 11, Patch: 9},
			want:    false,
		},
		{
			name:    "lower major version",
			version: FirmwareVersion{Major: 0, Minor: 99, Patch: 99},
			want:    false,
		},
		{
			name:    "version 1.0.0",
			version: FirmwareVersion{Major: 1, Minor: 0, Patch: 0},
			want:    false,
		},
		{
			name:    "version 1.11.0",
			version: FirmwareVersion{Major: 1, Minor: 11, Patch: 0},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.version.IsCompatible(); got != tt.want {
				t.Errorf("FirmwareVersion.IsCompatible() = %v, want %v (version: %v.%v.%v)",
					got, tt.want, tt.version.Major, tt.version.Minor, tt.version.Patch)
			}
		})
	}
}

func TestFirmwareVersion_String(t *testing.T) {
	tests := []struct {
		name    string
		version FirmwareVersion
		want    string
	}{
		{
			name:    "standard version",
			version: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:    "v1.12.0",
		},
		{
			name:    "higher version",
			version: FirmwareVersion{Major: 2, Minor: 5, Patch: 13},
			want:    "v2.5.13",
		},
		{
			name:    "zero version",
			version: FirmwareVersion{Major: 0, Minor: 0, Patch: 0},
			want:    "v0.0.0",
		},
		{
			name:    "version with build number",
			version: FirmwareVersion{Major: 2, Minor: 0, Patch: 0, Build: 1},
			want:    "v2.0.0-1",
		},
		{
			name:    "version with suffix only",
			version: FirmwareVersion{Major: 2, Minor: 0, Patch: 0, Suffix: "ls"},
			want:    "v2.0.0-ls",
		},
		{
			name:    "version with build and suffix",
			version: FirmwareVersion{Major: 2, Minor: 0, Patch: 0, Build: 1, Suffix: "ls"},
			want:    "v2.0.0-1-ls",
		},
		{
			name:    "version with multi-part suffix",
			version: FirmwareVersion{Major: 1, Minor: 12, Patch: 3, Build: 42, Suffix: "ls-beta"},
			want:    "v1.12.3-42-ls-beta",
		},
		{
			name:    "version with build metadata",
			version: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			want:    "v1.12.0+alarm",
		},
		{
			name:    "version with suffix and build metadata",
			version: FirmwareVersion{Major: 2, Minor: 0, Patch: 0, Build: 1, Suffix: "ls", BuildMetadata: "build42"},
			want:    "v2.0.0-1-ls+build42",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.version.String(); got != tt.want {
				t.Errorf("FirmwareVersion.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Helper function to check if a string contains a substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || stringContains(s, substr))
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFirmwareVersion_Compare(t *testing.T) {
	tests := []struct {
		name  string
		v1    FirmwareVersion
		v2    *FirmwareVersion
		want  int
	}{
		{
			name: "equal versions",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want: 0,
		},
		{
			name: "v1 major greater",
			v1:   FirmwareVersion{Major: 2, Minor: 0, Patch: 0},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			want: 1,
		},
		{
			name: "v1 major less",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			v2:   &FirmwareVersion{Major: 2, Minor: 0, Patch: 0},
			want: -1,
		},
		{
			name: "v1 minor greater",
			v1:   FirmwareVersion{Major: 1, Minor: 13, Patch: 0},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			want: 1,
		},
		{
			name: "v1 minor less",
			v1:   FirmwareVersion{Major: 1, Minor: 11, Patch: 0},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want: -1,
		},
		{
			name: "v1 patch greater",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 3},
			want: 1,
		},
		{
			name: "v1 patch less",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 3},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 5},
			want: -1,
		},
		{
			name: "v1 build greater",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 1},
			want: 1,
		},
		{
			name: "v1 build less",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 1},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2},
			want: -1,
		},
		{
			name: "equal with build",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2, Suffix: "ls"},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2, Suffix: "ls"},
			want: 0,
		},
		{
			name: "suffix ignored in comparison",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2, Suffix: "ls"},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2, Suffix: "beta"},
			want: 0,
		},
		{
			name: "compare with nil",
			v1:   FirmwareVersion{Major: 1, Minor: 0, Patch: 0},
			v2:   nil,
			want: 1,
		},
		{
			name: "version with zero build vs positive build",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 0},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 1},
			want: -1,
		},
		{
			name: "realistic example: v2.3.4-2-ls > v2.3.4-1-ls",
			v1:   FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 2, Suffix: "ls"},
			v2:   &FirmwareVersion{Major: 2, Minor: 3, Patch: 4, Build: 1, Suffix: "ls"},
			want: 1,
		},
		{
			name: "build metadata ignored: v1.12.0+alarm == v1.12.0",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want: 0,
		},
		{
			name: "build metadata ignored: v1.12.0+alarm == v1.12.0+other",
			v1:   FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			v2:   &FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "other"},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.v1.Compare(tt.v2); got != tt.want {
				t.Errorf("FirmwareVersion.Compare() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFirmwareVersion_IsNewerThan(t *testing.T) {
	tests := []struct {
		name      string
		available FirmwareVersion
		installed FirmwareVersion
		want      bool
	}{
		{
			name:      "newer base version triggers update",
			available: FirmwareVersion{Major: 1, Minor: 13, Patch: 0},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:      true,
		},
		{
			name:      "same version, no update",
			available: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:      false,
		},
		{
			name:      "older base version, no update",
			available: FirmwareVersion{Major: 1, Minor: 11, Patch: 0},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:      false,
		},
		{
			name:      "same base, metadata added: v1.12.0+alarm available, v1.12.0 installed",
			available: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:      true,
		},
		{
			name:      "same base, metadata removed: v1.12.0 available, v1.12.0+alarm installed",
			available: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			want:      true,
		},
		{
			name:      "same base and metadata, no update",
			available: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			want:      false,
		},
		{
			name:      "same base, different metadata: v1.12.0+alarm vs v1.12.0+other",
			available: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "alarm"},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0, BuildMetadata: "other"},
			want:      true,
		},
		{
			name:      "newer base with metadata beats older base without",
			available: FirmwareVersion{Major: 1, Minor: 13, Patch: 0, BuildMetadata: "alarm"},
			installed: FirmwareVersion{Major: 1, Minor: 12, Patch: 0},
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.available.IsNewerThan(&tt.installed); got != tt.want {
				t.Errorf("FirmwareVersion.IsNewerThan() = %v, want %v", got, tt.want)
			}
		})
	}
}

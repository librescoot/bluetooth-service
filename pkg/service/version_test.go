package service

import (
	"testing"
)

func TestParseFirmwareVersion(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMajor   int
		wantMinor   int
		wantPatch   int
		wantErr     bool
		errContains string
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

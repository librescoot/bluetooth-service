package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDataBrowserSetting(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		want, wantErr  bool
	}{
		{"missing", "", false, false},
		{"true", "[bluetooth]\ndata-browser = true\n", true, false},
		{"false", "[bluetooth]\ndata-browser = false\n", false, false},
		{"persisted string", "[bluetooth]\ndata-browser = \"true\"\n", true, false},
		{"invalid string", "[bluetooth]\ndata-browser = \"yes\"\n", false, true},
		{"invalid type", "[bluetooth]\ndata-browser = 1\n", false, true},
		{"broken toml", "[bluetooth\n", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.toml")
			if tc.contents != "" {
				if err := os.WriteFile(path, []byte(tc.contents), 0600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := loadDataBrowserSetting(path)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got %v, %v; want %v, error=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

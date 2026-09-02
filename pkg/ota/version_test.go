package ota

import "testing"

func TestVersionFromBundleID(t *testing.T) {
	cases := map[string]string{
		"librescoot-unu-mdb-v1.3.0":                         "v1.3.0",
		"librescoot-unu-dbc-nightly-20260101T000000.delta":  "nightly-20260101T000000",
		"librescoot-unu-mdb-testing-20260101T000000.mender": "testing-20260101T000000",
		"librescoot-unu-mdb":                                "",
		"librescoot-unu-mdb-":                               "",
		"x":                                                 "",
	}
	for id, want := range cases {
		if got := versionFromBundleID(id); got != want {
			t.Errorf("versionFromBundleID(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestSameVersion(t *testing.T) {
	cases := []struct {
		target, installed string
		want              bool
	}{
		{"v1.3.0", "1.3.0", true},
		{"v1.3.0", "v1.3.0", true},
		{"v1.3.0", "1.4.0", false},
		{"v1.3.0", "1.2.0", false},
		{"nightly-20260101T000000", "nightly-20260101t000000", true},
		{"nightly-20260101T000000", "20260101T000000", true},
		{"nightly-20260101T000000", "nightly-20260102T000000", false},
		{"testing-20260101T000000", "testing-20260101T000000", true},
		{"v1.3.0", "testing-20260101T000000", false},
		{"v1.3.0", "", false},
		{"v1.3.0", "unknown", false},
		{"", "1.3.0", false},
		{"garbage", "garbage", false},
	}
	for _, tc := range cases {
		if got := sameVersion(tc.target, tc.installed); got != tc.want {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", tc.target, tc.installed, got, tc.want)
		}
	}
}

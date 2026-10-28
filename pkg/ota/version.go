package ota

import "strings"

// versionFromBundleID extracts the version token from a bundle ID such as
// "librescoot-unu-mdb-v1.3.0" or "librescoot-unu-dbc-nightly-20260101T000000.delta".
// The component prefix ends at the third hyphen. Returns "" if the ID does not
// have that shape.
func versionFromBundleID(id string) string {
	for _, suffix := range []string{".mender", ".delta"} {
		if strings.HasSuffix(id, suffix) {
			id = strings.TrimSuffix(id, suffix)
			break
		}
	}
	parts := strings.SplitN(id, "-", 4)
	if len(parts) < 4 || parts[3] == "" {
		return ""
	}
	return parts[3]
}

// channelOf returns the channel implied by a version token: "nightly" or
// "testing" for prefixed tokens, "stable" for bare semver, "" if unknown.
func channelOf(v string) string {
	switch {
	case strings.HasPrefix(v, "nightly-"):
		return "nightly"
	case strings.HasPrefix(v, "testing-"):
		return "testing"
	case strings.HasPrefix(v, "v"):
		return "stable"
	}
	return ""
}

// sameVersion reports whether the bundle's target version is what the board
// already runs. installed is version:<comp>[version_id] as version-service
// copies it from os-release, which lacks the "v" (stable) or "<channel>-"
// (older nightly/testing images) prefix that filenames carry, so it is
// normalized to the target's channel before comparing. Unknown on either
// side means "cannot tell" and reports false.
func sameVersion(target, installed string) bool {
	target = strings.ToLower(strings.TrimSpace(target))
	installed = strings.ToLower(strings.TrimSpace(installed))
	if target == "" || installed == "" || installed == "unknown" {
		return false
	}
	switch channelOf(target) {
	case "nightly", "testing":
		ch := channelOf(target)
		if !strings.HasPrefix(installed, ch+"-") && installed[0] >= '0' && installed[0] <= '9' {
			installed = ch + "-" + installed
		}
	case "stable":
		if !strings.HasPrefix(installed, "v") {
			installed = "v" + installed
		}
	default:
		return false
	}
	return target == installed
}

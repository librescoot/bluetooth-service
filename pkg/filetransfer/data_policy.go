package filetransfer

import "strings"

func restrictedDataDirectory(path string) bool {
	switch path {
	case ".", "ota", "ota/mdb", "radio-gaga":
		return true
	default:
		return false
	}
}

func restrictedDataPath(path string) bool {
	if restrictedDataDirectory(path) || path == "radio-gaga/config.yaml" {
		return true
	}
	if !strings.HasPrefix(path, "ota/mdb/") {
		return false
	}
	name := strings.TrimPrefix(path, "ota/mdb/")
	return validComponent(name) && !strings.HasPrefix(name, ".") &&
		(strings.HasSuffix(name, ".mender") || strings.HasSuffix(name, ".delta") ||
			strings.HasSuffix(name, ".hdelta"))
}

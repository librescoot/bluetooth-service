package filetransfer

import (
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

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

// A broker selected in the configuration must not gain a route to rewrite the
// configuration, execute code, or change Redis directly.
func validateRadioGagaConfig(file *os.File, size uint64) error {
	const maxConfigSize = 1 << 20
	if size == 0 || size > maxConfigSize {
		return os.ErrPermission
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, int64(size)))
	if err != nil {
		return err
	}
	var config map[interface{}]interface{}
	if err := yaml.UnmarshalStrict(data, &config); err != nil {
		return os.ErrPermission
	}
	if environment, ok := config["environment"].(string); !ok || environment != "production" {
		return os.ErrPermission
	}
	if _, present := config["bootstrap"]; present {
		return os.ErrPermission
	}
	scooter, ok := config["scooter"].(map[interface{}]interface{})
	if !ok {
		return os.ErrPermission
	}
	identifier, idOK := scooter["identifier"].(string)
	token, tokenOK := scooter["token"].(string)
	if !idOK || !tokenOK || identifier == "" || token == "" {
		return os.ErrPermission
	}
	commands, ok := config["commands"].(map[interface{}]interface{})
	if !ok {
		return os.ErrPermission
	}
	for _, name := range []string{"shell", "redis", "self_update", "config:set", "config:del", "config:save", "txn:replace", "update_ca_cert", "migrate_broker"} {
		command, ok := commands[name].(map[interface{}]interface{})
		if !ok || command["disabled"] != true {
			return os.ErrPermission
		}
	}
	return nil
}

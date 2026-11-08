package main

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

func loadDataBrowserSetting(path string) (bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	var settings struct {
		Bluetooth map[string]any `toml:"bluetooth"`
	}
	if _, err := toml.NewDecoder(f).Decode(&settings); err != nil {
		return false, err
	}
	switch value := settings.Bluetooth["data-browser"].(type) {
	case nil:
		return false, nil
	case bool:
		return value, nil
	case string:
		switch value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, fmt.Errorf("bluetooth.data-browser must be true or false")
}

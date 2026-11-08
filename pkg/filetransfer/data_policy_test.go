package filetransfer

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const safeRadioConfig = `environment: production
scooter: {identifier: scooter, token: secret}
commands:
  shell: {disabled: true}
  redis: {disabled: true}
  self_update: {disabled: true}
  config:set: {disabled: true}
  config:del: {disabled: true}
  config:save: {disabled: true}
  txn:replace: {disabled: true}
  update_ca_cert: {disabled: true}
  migrate_broker: {disabled: true}
`

func TestRestrictedDataPath(t *testing.T) {
	for _, p := range []string{".", "ota", "ota/mdb", "ota/mdb/update.mender", "ota/mdb/update.delta", "radio-gaga", "radio-gaga/config.yaml"} {
		if !restrictedDataPath(p) {
			t.Errorf("%q denied", p)
		}
	}
	for _, p := range []string{"settings.toml", "usb.drive", "onboot.sh", "wireguard", "ota/dbc", "ota/mdb/.hidden.mender", "ota/mdb/../settings.toml", "ota/mdb/sub/file.mender", "ota/mdb/install.sh", "radio-gaga/other.yaml", "radio-gaga/config.yaml/child"} {
		if restrictedDataPath(p) {
			t.Errorf("%q allowed", p)
		}
	}
}

func TestRestrictedRootDeniesOtherData(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"settings.toml", "onboot.sh", "usb.drive"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(nil, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetRestrictedDataRoot(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"settings.toml", "onboot.sh", "usb.drive", "wireguard/private.key"} {
		if _, err := s.checkedInfo(path); !errors.Is(err, os.ErrPermission) {
			t.Errorf("%q: %v", path, err)
		}
	}
}

func TestRestrictedRootHidesAndRejectsPathsOverProtocol(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "settings.toml"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ota", "mdb"), 0700); err != nil {
		t.Fatal(err)
	}
	writer := &testWriter{frames: make(chan testFrame, 128)}
	s := New(func() Writer { return writer }, nil, Hooks{})
	t.Cleanup(s.Close)
	if err := s.SetRestrictedDataRoot(root); err != nil {
		t.Fatal(err)
	}
	dir, _ := openRootAndResolve(t, s, writer, "ota", 1)
	name := "settings.toml"
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpResolveName, ID: 10, Budget: 128, DirectoryID: dir, NameTotal: uint16(len(name)), Name: name}))
	if body := message(t, writer, 10); len(body) != 2 || body[0] != RespError || body[1] != ErrDenied {
		t.Fatalf("protected resolve = %x", body)
	}
	s.HandleControl(EncodeRequest(Request{Version: Version2, Op: OpListDir, ID: 11, Budget: 128, DirectoryID: dir}))
	if body := message(t, writer, 11); len(body) < 28 || string(body[28:]) != "ota" {
		t.Fatalf("root listing = %x", body)
	}
}

func TestRadioGagaUploadRequiresSafeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		accepted   bool
	}{
		{"safe", safeRadioConfig, true},
		{"development", strings.Replace(safeRadioConfig, "production", "development", 1), false},
		{"bootstrap", safeRadioConfig + "bootstrap: {code: attack}\n", false},
		{"no token", strings.Replace(safeRadioConfig, "token: secret", "token: ''", 1), false},
		{"shell", strings.Replace(safeRadioConfig, "shell: {disabled: true}", "shell: {disabled: false}", 1), false},
		{"redis missing", strings.Replace(safeRadioConfig, "  redis: {disabled: true}\n", "", 1), false},
		{"duplicate", safeRadioConfig + "environment: development\n", false},
		{"malformed", "[", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "radio-gaga"), 0700); err != nil {
				t.Fatal(err)
			}
			s := New(nil, nil, Hooks{})
			t.Cleanup(s.Close)
			if err := s.SetRestrictedDataRoot(root); err != nil {
				t.Fatal(err)
			}
			data := []byte(tc.data)
			req := Request{Intent: IntentCreate, Chunk: 8, Size: uint64(len(data)), Hash: sha256.Sum256(data)}
			f, _, err := s.beginDataWrite(req, "radio-gaga/config.yaml", func() error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			f.root, f.borrowedRoot = s.dataRoot, true
			defer f.close()
			if _, err := f.file.WriteAt(data, 0); err != nil {
				t.Fatal(err)
			}
			err = f.commitData()
			if (err == nil) != tc.accepted {
				t.Fatalf("commit error = %v, accepted = %v", err, tc.accepted)
			}
			_, err = os.Stat(filepath.Join(root, "radio-gaga", "config.yaml"))
			if (err == nil) != tc.accepted {
				t.Fatalf("published: %v", err)
			}
		})
	}
}

package filetransfer

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestFinalPublicationCheckRejectsRenamedParent(t *testing.T) {
	for _, intent := range []byte{IntentCreate, IntentReplace} {
		name := map[byte]string{IntentCreate: "create.bin", IntentReplace: "replace.bin"}[intent]
		t.Run(name, func(t *testing.T) {
			rootPath := t.TempDir()
			parentPath := filepath.Join(rootPath, "nested")
			if err := os.Mkdir(parentPath, 0700); err != nil {
				t.Fatal(err)
			}
			oldContent := []byte(nil)
			if intent == IntentReplace {
				if err := os.WriteFile(filepath.Join(parentPath, name), oldContent, 0600); err != nil {
					t.Fatal(err)
				}
			}
			server := New(func() Writer { return nil }, nil, Hooks{})
			defer server.Close()
			if err := server.SetDataRoot(rootPath); err != nil {
				t.Fatal(err)
			}
			parentInfo, err := server.dataRoot.Lstat("nested")
			if err != nil {
				t.Fatal(err)
			}
			node := &dataNode{path: "nested/" + name, parentPath: "nested", parentIdentity: identityOf(parentInfo), kind: 0}
			oldHash := sha256.Sum256(oldContent)
			if intent == IntentReplace {
				oldInfo, err := server.dataRoot.Lstat(node.path)
				if err != nil {
					t.Fatal(err)
				}
				node.kind = 1
				node.identity = identityOf(oldInfo)
			}
			stageDir := ".ble-transfer"
			if err := server.dataRoot.Mkdir(stageDir, 0700); err != nil {
				t.Fatal(err)
			}
			stagePath := stageDir + "/pending.part"
			stage, err := server.dataRoot.OpenFile(stagePath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
			if err != nil {
				t.Fatal(err)
			}
			stored := &storedFile{root: server.dataRoot, borrowedRoot: true, file: stage, info: FileInfo{Name: name, Hash: sha256.Sum256(nil)}, isData: true, targetPath: node.path, stagePath: stagePath, intent: intent, oldSize: uint64(len(oldContent)), oldHash: oldHash}
			if intent == IntentReplace {
				target, err := server.dataRoot.Lstat(node.path)
				if err != nil {
					t.Fatal(err)
				}
				st := target.Sys().(*syscall.Stat_t)
				stored.oldIdentity = identityOf(target)
				stored.oldUID = int(st.Uid)
				stored.oldGID = int(st.Gid)
				stored.oldMode = target.Mode()
			}
			moved := filepath.Join(t.TempDir(), "moved")
			checks := 0
			finalCheck := 3
			if intent == IntentReplace {
				finalCheck = 5
			}
			stored.check = func() error {
				checks++
				if checks == finalCheck {
					if err := os.Rename(parentPath, moved); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(parentPath, 0700); err != nil {
						t.Fatal(err)
					}
				}
				_, err := server.validateNode(node)
				return err
			}
			err = stored.commitData()
			if !errors.Is(err, errChanged) {
				t.Fatalf("commit error %v (checks=%d)", err, checks)
			}
			if checks != finalCheck {
				t.Fatalf("final check calls=%d want %d", checks, finalCheck)
			}
			if _, err := server.dataRoot.Lstat(node.path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published at replaced path: %v", err)
			}
			if intent == IntentReplace {
				if got, err := os.ReadFile(filepath.Join(moved, name)); err != nil || string(got) != string(oldContent) {
					t.Fatalf("original replacement target changed: %q %v", got, err)
				}
			} else if _, err := os.Stat(filepath.Join(moved, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("CREATE published into moved parent: %v", err)
			}
			if _, err := server.dataRoot.Lstat(stagePath); err != nil {
				t.Fatalf("matching private partial not retained: %v", err)
			}
		})
	}
}

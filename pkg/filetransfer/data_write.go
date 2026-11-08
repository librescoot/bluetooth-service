package filetransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	maxDataPartialFiles = 512
	maxDataPartialAge   = 7 * 24 * time.Hour
)

func dataStageName(path string, req Request, oldIdentity fileIdentity) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte{req.Intent})
	h.Write(req.Hash[:])
	h.Write(req.OldHash[:])
	_, _ = fmt.Fprintf(h, ":%d:%d:%d:%d:%d:%d:%d", req.Size, req.OldSize, oldIdentity.dev, oldIdentity.ino, oldIdentity.mode, oldIdentity.size, oldIdentity.modified)
	return ".ble-transfer/" + hex.EncodeToString(h.Sum(nil)) + ".part"
}

func (s *Server) beginDataWrite(req Request, path string, check func() error) (*storedFile, uint64, error) {
	if req.Size > maxFileSize || req.Chunk == 0 || req.Chunk > MaxPayload-DataHeader {
		return nil, 0, os.ErrPermission
	}
	var oldIdentity fileIdentity
	var oldUID, oldGID int
	var oldMode os.FileMode
	if req.Intent == IntentReplace {
		target, err := s.checkedInfo(path)
		if err != nil {
			return nil, 0, err
		}
		if !target.Mode().IsRegular() || uint64(target.Size()) != req.OldSize {
			return nil, 0, errChanged
		}
		old, err := s.openDataRead(path, check)
		if err != nil {
			return nil, 0, err
		}
		if old.info.Hash != req.OldHash {
			old.close()
			return nil, 0, errChanged
		}
		old.close()
		oldIdentity = identityOf(target)
		st, ok := target.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, 0, os.ErrPermission
		}
		oldUID, oldGID, oldMode = int(st.Uid), int(st.Gid), target.Mode()
	} else if _, err := s.checkedInfo(path); err == nil {
		return nil, 0, os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	if err := s.dataRoot.Mkdir(".ble-transfer", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, 0, err
	}
	stageDir, err := s.dataRoot.Lstat(".ble-transfer")
	if err != nil || !stageDir.IsDir() || stageDir.Mode()&os.ModeSymlink != 0 {
		return nil, 0, os.ErrPermission
	}
	if err := s.dataRoot.Chmod(".ble-transfer", 0700); err != nil {
		return nil, 0, err
	}
	if st, ok := stageDir.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Geteuid() {
		return nil, 0, os.ErrPermission
	}
	stage := dataStageName(path, req, oldIdentity)
	_, stageErr := s.dataRoot.Lstat(stage)
	newStage := errors.Is(stageErr, os.ErrNotExist)
	if newStage {
		if err := cleanDataStages(s.dataRoot, time.Now()); err != nil {
			return nil, 0, err
		}
	} else if stageErr != nil {
		return nil, 0, stageErr
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.dataPath, &fs); err != nil {
		return nil, 0, err
	}
	flags := os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if newStage {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := s.dataRoot.OpenFile(stage, flags, 0600)
	if err != nil {
		return nil, 0, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, 0, err
	}
	info, err := f.Stat()
	st, statOK := (*syscall.Stat_t)(nil), false
	if err == nil {
		st, statOK = info.Sys().(*syscall.Stat_t)
	}
	if err != nil || !info.Mode().IsRegular() || !statOK || int(st.Uid) != os.Geteuid() || info.Size() < 0 || uint64(info.Size()) > req.Size {
		f.Close()
		return nil, 0, os.ErrPermission
	}
	offset := uint64(info.Size())
	offset -= offset % uint64(req.Chunk)
	if fs.Bavail*uint64(fs.Bsize) < req.Size-offset+freeMargin {
		err := reclaimDataSpace(s.dataRoot, s.dataPath, filepath.Base(stage), req.Size-offset+freeMargin, fs.Bavail*uint64(fs.Bsize))
		if err != nil {
			f.Close()
			if newStage {
				_ = s.dataRoot.Remove(stage)
			}
			return nil, 0, err
		}
	}
	if err := f.Truncate(int64(offset)); err != nil {
		f.Close()
		return nil, 0, err
	}
	stored := &storedFile{file: f, info: FileInfo{Name: filepath.Base(path), Size: req.Size, Hash: req.Hash}, check: check, isData: true, targetPath: path, stagePath: stage, intent: req.Intent, oldSize: req.OldSize, oldHash: req.OldHash, oldIdentity: oldIdentity, oldUID: oldUID, oldGID: oldGID, oldMode: oldMode}
	if err := check(); err != nil {
		stored.close()
		return nil, 0, err
	}
	return stored, offset, nil
}

func cleanDataStages(root *os.Root, now time.Time) error {
	for {
		dir, err := root.Open(".ble-transfer")
		if err != nil {
			return err
		}
		entries, readErr := dir.ReadDir(maxDataPartialFiles + 1)
		_ = dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		remaining := len(entries)
		oldest := ""
		var oldestTime time.Time
		for _, entry := range entries {
			name := entry.Name()
			if len(name) != 69 || !strings.HasSuffix(name, ".part") {
				continue
			}
			if _, err := hex.DecodeString(name[:64]); err != nil {
				continue
			}
			info, err := root.Lstat(".ble-transfer/" + name)
			if errors.Is(err, os.ErrNotExist) {
				remaining--
				continue
			}
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !info.Mode().IsRegular() || !ok || int(st.Uid) != os.Geteuid() {
				continue
			}
			if info.ModTime().Before(now.Add(-maxDataPartialAge)) {
				if err := root.Remove(".ble-transfer/" + name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				remaining--
				continue
			}
			if oldest == "" || info.ModTime().Before(oldestTime) {
				oldest, oldestTime = name, info.ModTime()
			}
		}
		if remaining < maxDataPartialFiles {
			if len(entries) == maxDataPartialFiles+1 {
				continue
			}
			return nil
		}
		if oldest == "" {
			return syscall.ENOSPC
		}
		if err := root.Remove(".ble-transfer/" + oldest); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
}

type dataStageCandidate struct {
	name      string
	modified  time.Time
	allocated uint64
}

func reclaimDataSpace(root *os.Root, dataPath, exclude string, required, available uint64) error {
	dir, err := root.Open(".ble-transfer")
	if err != nil {
		return err
	}
	defer dir.Close()
	var candidates []dataStageCandidate
	for {
		entries, readErr := dir.ReadDir(64)
		for _, entry := range entries {
			name := entry.Name()
			if name == exclude || len(name) != 69 || !strings.HasSuffix(name, ".part") {
				continue
			}
			if _, err := hex.DecodeString(name[:64]); err != nil {
				continue
			}
			info, err := root.Lstat(".ble-transfer/" + name)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !info.Mode().IsRegular() || !ok || int(st.Uid) != os.Geteuid() || st.Blocks <= 0 {
				continue
			}
			candidates = append(candidates, dataStageCandidate{name, info.ModTime(), uint64(st.Blocks) * 512})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	potential := available
	for _, candidate := range candidates {
		potential += candidate.allocated
		if potential >= required {
			break
		}
	}
	if potential < required {
		return syscall.ENOSPC
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modified.Before(candidates[j].modified) })
	for _, candidate := range candidates {
		if err := root.Remove(".ble-transfer/" + candidate.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var fs syscall.Statfs_t
		if err := syscall.Statfs(dataPath, &fs); err != nil {
			return err
		}
		if fs.Bavail*uint64(fs.Bsize) >= required {
			return nil
		}
	}
	return syscall.ENOSPC
}

func (f *storedFile) commitData() error {
	if err := f.file.Sync(); err != nil {
		return err
	}
	info, err := f.file.Stat()
	if err != nil || info.Size() < 0 || uint64(info.Size()) != f.info.Size {
		return errChanged
	}
	hash, err := digest(f.file, f.info.Size, f.check)
	if err != nil {
		return err
	}
	if hash != f.info.Hash {
		return errChecksum
	}
	root := f.root
	if root == nil {
		return os.ErrPermission
	}
	stageInfo, err := root.Lstat(f.stagePath)
	if err != nil || !stageInfo.Mode().IsRegular() {
		return errChanged
	}
	openStageInfo, err := f.file.Stat()
	if err != nil || identityOf(stageInfo) != identityOf(openStageInfo) {
		return errChanged
	}
	// Re-resolve every ancestor under the fixed /data root immediately before publication.
	parts := stringsSplitPath(f.targetPath)
	for i := range parts {
		cur := joinParts(parts[:i+1])
		current, e := root.Lstat(cur)
		if e != nil {
			if f.intent == IntentCreate && i == len(parts)-1 && errors.Is(e, os.ErrNotExist) {
				break
			}
			return errChanged
		}
		if current.Mode()&os.ModeSymlink != 0 {
			return os.ErrPermission
		}
		if i < len(parts)-1 && !current.IsDir() {
			return os.ErrPermission
		}
	}
	if f.intent == IntentCreate {
		if f.check != nil {
			if err := f.check(); err != nil {
				return err
			}
		}
		if err := root.Link(f.stagePath, f.targetPath); err != nil {
			return err
		}
		if err := root.Remove(f.stagePath); err != nil {
			return err
		}
	} else {
		current, err := root.Lstat(f.targetPath)
		if err != nil || !current.Mode().IsRegular() || identityOf(current) != f.oldIdentity || uint64(current.Size()) != f.oldSize {
			return errChanged
		}
		old, err := root.OpenFile(f.targetPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return errChanged
		}
		oldHash, hashErr := digest(old, f.oldSize, f.check)
		old.Close()
		if hashErr != nil {
			return hashErr
		}
		if oldHash != f.oldHash {
			return errChanged
		}
		if err := f.file.Chown(f.oldUID, f.oldGID); err != nil {
			return fmt.Errorf("preserve replacement ownership: %w", err)
		}
		mode := f.oldMode.Perm() | f.oldMode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
		if err := f.file.Chmod(mode); err != nil {
			return err
		}
		if f.check != nil {
			if err := f.check(); err != nil {
				return err
			}
		}
		if err := root.Rename(f.stagePath, f.targetPath); err != nil {
			return err
		}
	}
	return syncRoot(root)
}

func stringsSplitPath(p string) []string { return strings.Split(p, "/") }
func joinParts(parts []string) string    { return filepath.Join(parts...) }

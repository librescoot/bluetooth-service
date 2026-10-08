package filetransfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const maxFileSize = uint64(4 << 30)
const freeMargin = uint64(32 << 20)

var errCancelled = errors.New("transfer cancelled")
var errChecksum = errors.New("checksum mismatch")
var errChanged = errors.New("file changed while hashing")
var ErrTransportBusy = errors.New("bulk transfer busy")

type Store struct {
	Dir      string
	Writable bool
	Suffix   string
}
type FileInfo struct {
	Name     string
	Size     uint64
	Modified int64
	Hash     [32]byte
}
type identity struct {
	Name string
	Size uint64
	Hash string
}
type storedFile struct {
	root    *os.Root
	file    *os.File
	info    FileInfo
	partial string
	check   func() error
}

func (f *storedFile) close() {
	if f.file != nil {
		f.file.Close()
	}
	if f.root != nil {
		f.root.Close()
	}
}

func (s Store) open() (*os.Root, error) {
	if s.Writable {
		if err := os.MkdirAll(s.Dir, 0700); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("store is not a directory")
	}
	return os.OpenRoot(s.Dir)
}
func (s Store) allowed(name string) bool {
	return ValidName(name) && (s.Suffix == "" || strings.HasSuffix(name, s.Suffix))
}
func (s Store) List() ([]FileInfo, error) {
	root, err := s.open()
	if errors.Is(err, os.ErrNotExist) {
		return []FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(2049)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 2048 {
		return nil, fmt.Errorf("too many files")
	}
	files := []FileInfo{}
	for _, e := range entries {
		if !s.allowed(e.Name()) || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) > maxFileSize {
			continue
		}
		files = append(files, FileInfo{Name: e.Name(), Size: uint64(info.Size()), Modified: info.ModTime().Unix()})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Modified == files[j].Modified {
			return files[i].Name < files[j].Name
		}
		return files[i].Modified > files[j].Modified
	})
	return files, nil
}

type checkedReader struct {
	io.Reader
	check func() error
}

func (r checkedReader) Read(p []byte) (int, error) {
	if r.check != nil {
		if err := r.check(); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(p)
}
func digest(file *os.File, size uint64, check func() error) ([32]byte, error) {
	var result [32]byte
	h := sha256.New()
	n, err := io.CopyBuffer(h, checkedReader{io.NewSectionReader(file, 0, int64(size)), check}, make([]byte, 64<<10))
	if err != nil {
		return result, err
	}
	if uint64(n) != size {
		return result, io.ErrUnexpectedEOF
	}
	if check != nil {
		if err := check(); err != nil {
			return result, err
		}
	}
	copy(result[:], h.Sum(nil))
	return result, nil
}
func (s Store) Read(name string) (*storedFile, error) { return s.read(name, nil) }
func (s Store) read(name string, check func() error) (*storedFile, error) {
	if check != nil {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if !s.allowed(name) {
		return nil, os.ErrPermission
	}
	root, err := s.open()
	if err != nil {
		return nil, err
	}
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		root.Close()
		return nil, err
	}
	result := &storedFile{root: root, file: f, check: check}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) > maxFileSize {
		result.close()
		return nil, os.ErrPermission
	}
	result.info = FileInfo{Name: name, Size: uint64(info.Size()), Modified: info.ModTime().Unix()}
	hash, err := digest(f, result.info.Size, check)
	if err != nil {
		result.close()
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		result.close()
		return nil, errChanged
	}
	result.info.Hash = hash
	return result, nil
}
func (s Store) Write(req Request) (*storedFile, uint64, error) { return s.write(req, nil) }
func (s Store) write(req Request, check func() error) (*storedFile, uint64, error) {
	if check != nil {
		if err := check(); err != nil {
			return nil, 0, err
		}
	}
	if !s.Writable || !s.allowed(req.Name) || req.Size > maxFileSize || req.Chunk == 0 || req.Chunk > MaxPayload-DataHeader {
		return nil, 0, os.ErrPermission
	}
	root, err := s.open()
	if err != nil {
		return nil, 0, err
	}
	result := &storedFile{root: root, check: check, info: FileInfo{Name: req.Name, Size: req.Size, Hash: req.Hash}}
	fail := func(err error) (*storedFile, uint64, error) { result.close(); return nil, 0, err }
	if _, err := root.Lstat(req.Name); err == nil {
		return fail(os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	if err := root.MkdirAll(".partial", 0700); err != nil {
		return fail(err)
	}
	result.partial = ".partial/" + req.Name + ".part"
	scPath := ".partial/" + req.Name + ".json"
	expected := identity{Name: req.Name, Size: req.Size, Hash: hex.EncodeToString(req.Hash[:])}
	var existing identity
	if sc, err := root.OpenFile(scPath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0); err == nil {
		if info, err := sc.Stat(); err == nil && info.Mode().IsRegular() && info.Size() <= 1024 {
			if data, err := io.ReadAll(io.LimitReader(sc, 1025)); err == nil {
				_ = json.Unmarshal(data, &existing)
			}
		}
		sc.Close()
	}
	resume := existing == expected
	var fs syscall.Statfs_t
	if err := syscall.Statfs(s.Dir, &fs); err != nil {
		return fail(err)
	}
	need := req.Size
	if resume {
		if info, err := root.Lstat(result.partial); err == nil && info.Mode().IsRegular() && info.Size() >= 0 && uint64(info.Size()) <= req.Size {
			need -= uint64(info.Size())
		}
	}
	if fs.Bavail*uint64(fs.Bsize) < need+freeMargin {
		return fail(syscall.ENOSPC)
	}
	flags := os.O_CREATE | os.O_RDWR | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if !resume {
		flags |= os.O_TRUNC
	}
	f, err := root.OpenFile(result.partial, flags, 0600)
	if err != nil {
		return fail(err)
	}
	result.file = f
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return fail(os.ErrPermission)
	}
	offset := uint64(info.Size())
	if offset > req.Size {
		offset = 0
	}
	offset -= offset % uint64(req.Chunk)
	if err := f.Truncate(int64(offset)); err != nil {
		return fail(err)
	}
	data, _ := json.Marshal(expected)
	temp := scPath + ".tmp"
	sc, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fail(err)
	}
	_, writeErr := sc.Write(data)
	syncErr := sc.Sync()
	closeErr := sc.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		root.Remove(temp)
		return fail(err)
	}
	if err := root.Rename(temp, scPath); err != nil {
		return fail(err)
	}
	if err := syncRoot(root); err != nil {
		return fail(err)
	}
	return result, offset, nil
}
func syncRoot(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (f *storedFile) commit() error {
	if err := f.file.Sync(); err != nil {
		return err
	}
	info, err := f.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 0 || uint64(info.Size()) != f.info.Size {
		return errChanged
	}
	hash, err := digest(f.file, f.info.Size, f.check)
	if err != nil {
		return err
	}
	if hash != f.info.Hash {
		return errChecksum
	}
	after, err := f.file.Stat()
	if err != nil {
		return err
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return errChanged
	}
	// Link publishes without replacing an existing artifact, even under a race.
	if err := f.root.Link(f.partial, f.info.Name); err != nil {
		return err
	}
	if err := f.root.Remove(f.partial); err != nil {
		return err
	}
	if err := f.root.Remove(filepath.Join(".partial", f.info.Name+".json")); err != nil {
		return err
	}
	return syncRoot(f.root)
}

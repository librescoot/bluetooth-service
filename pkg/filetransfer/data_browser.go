package filetransfer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxDataDirectories = 64
	maxDataNodes       = 256
	listChunkSize      = 84
)

type fileIdentity struct {
	dev, ino uint64
	mode     os.FileMode
	size     int64
	modified int64
}

// identity pins the directory object; a listing snapshot separately tracks its changing contents.
type dataDirectory struct {
	path     string
	identity fileIdentity
	touched  time.Time
}
type dataNode struct {
	path           string
	parentPath     string
	parentIdentity fileIdentity
	kind           byte
	identity       fileIdentity
	touched        time.Time
}
type resolveReplay struct {
	dir         [16]byte
	request     uint32
	total       uint16
	finalOffset uint16
	finalChunk  string
	body        []byte
	nodeID      [16]byte
	touched     time.Time
}
type pendingResolve struct {
	dir     [16]byte
	total   uint16
	next    uint16
	data    []byte
	request uint32
	touched time.Time
}
type directoryListing struct {
	dir      [16]byte
	snapshot fileIdentity
	iterator *os.File
	cursor   uint32
	position uint32
	name     string
	kind     byte
	size     uint64
	modified int64
	request  uint32
	end      bool
	touched  time.Time
}

func (s *Server) SetDataRoot(path string) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return os.ErrPermission
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() || identityOf(before) != identityOf(info) {
		root.Close()
		if err != nil {
			return err
		}
		return os.ErrPermission
	}
	s.resetDataHandles()
	if s.dataRoot != nil {
		s.dataRoot.Close()
	}
	s.dataRoot, s.dataPath = root, path
	s.dataDirs = make(map[[16]byte]*dataDirectory)
	s.dataNodes = make(map[[16]byte]*dataNode)
	return nil
}

func (s *Server) clearListing() {
	if s.listState != nil && s.listState.iterator != nil {
		_ = s.listState.iterator.Close()
	}
	s.listState = nil
}

func (s *Server) resetDataHandles() {
	s.clearListing()
	s.dataDirs = make(map[[16]byte]*dataDirectory)
	s.dataNodes = make(map[[16]byte]*dataNode)
	s.pendingName = nil
	s.resolveReplay = nil
	s.listState = nil
}

func identityOf(info os.FileInfo) fileIdentity {
	id := fileIdentity{mode: info.Mode(), size: info.Size(), modified: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), st.Ino
	}
	return id
}

func sameDirectoryObject(a, b fileIdentity) bool {
	return a.dev == b.dev && a.ino == b.ino && a.mode == b.mode
}

func (s *Server) checkedInfo(path string) (os.FileInfo, error) {
	if s.dataRoot == nil {
		return nil, os.ErrPermission
	}
	if path == "." {
		return s.dataRoot.Stat(".")
	}
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if !validComponent(part) {
			return nil, os.ErrPermission
		}
		current := strings.Join(parts[:i+1], "/")
		info, err := s.dataRoot.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrPermission
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, os.ErrPermission
		}
	}
	return s.dataRoot.Lstat(path)
}

func (s *Server) newID() ([16]byte, error) {
	var id [16]byte
	_, err := io.ReadFull(s.idSource, id[:])
	return id, err
}
func (s *Server) addDirectory(path string) ([16]byte, error) {
	var zero [16]byte
	if len(s.dataDirs) >= maxDataDirectories {
		s.evictDirectory()
	}
	info, err := s.checkedInfo(path)
	if err != nil {
		return zero, err
	}
	if !info.IsDir() {
		return zero, os.ErrPermission
	}
	var id [16]byte
	for {
		id, err = s.newID()
		if err != nil {
			return zero, err
		}
		_, dirExists := s.dataDirs[id]
		_, nodeExists := s.dataNodes[id]
		if !dirExists && !nodeExists {
			break
		}
	}
	s.dataDirs[id] = &dataDirectory{path: path, identity: identityOf(info), touched: time.Now()}
	return id, nil
}
func (s *Server) evictDirectory() {
	var key [16]byte
	var oldest time.Time
	for id, d := range s.dataDirs {
		if oldest.IsZero() || d.touched.Before(oldest) {
			key, oldest = id, d.touched
		}
	}
	delete(s.dataDirs, key)
	if s.listState != nil && s.listState.dir == key {
		s.clearListing()
	}
}
func (s *Server) addNode(path string, kind byte, info os.FileInfo) ([16]byte, error) {
	var zero [16]byte
	if len(s.dataNodes) >= maxDataNodes {
		var key [16]byte
		var oldest time.Time
		for id, n := range s.dataNodes {
			if s.resolveReplay != nil && id == s.resolveReplay.nodeID {
				continue
			}
			if oldest.IsZero() || n.touched.Before(oldest) {
				key, oldest = id, n.touched
			}
		}
		delete(s.dataNodes, key)
	}
	var id [16]byte
	var err error
	for {
		id, err = s.newID()
		if err != nil {
			return zero, err
		}
		_, dirExists := s.dataDirs[id]
		_, nodeExists := s.dataNodes[id]
		if !dirExists && !nodeExists {
			break
		}
	}
	n := &dataNode{path: path, kind: kind, touched: time.Now()}
	if info != nil {
		n.identity = identityOf(info)
	}
	s.dataNodes[id] = n
	return id, nil
}
func appendID(p []byte, id [16]byte) []byte { return append(p, id[:]...) }

func (s *Server) expireDataHandles() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for id, d := range s.dataDirs {
		if d.touched.Before(cutoff) {
			delete(s.dataDirs, id)
		}
	}
	for id, n := range s.dataNodes {
		if n.touched.Before(cutoff) {
			delete(s.dataNodes, id)
		}
	}
	if s.pendingName != nil && s.pendingName.touched.Before(time.Now().Add(-30*time.Second)) {
		s.pendingName = nil
	}
	if s.resolveReplay != nil && s.resolveReplay.touched.Before(time.Now().Add(-30*time.Second)) {
		s.resolveReplay = nil
	}
	if s.listState != nil && s.listState.touched.Before(cutoff) {
		s.clearListing()
	}
}

func (s *Server) validateNode(n *dataNode) (os.FileInfo, error) {
	n.touched = time.Now()
	parent, err := s.checkedInfo(n.parentPath)
	if err != nil || !parent.IsDir() || !sameDirectoryObject(identityOf(parent), n.parentIdentity) {
		return nil, errChanged
	}
	info, err := s.checkedInfo(n.path)
	if n.kind == 0 {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err == nil {
			return info, errChanged
		}
		return nil, err
	}
	if err != nil || identityOf(info) != n.identity {
		return nil, errChanged
	}
	return info, nil
}

func (s *Server) controlV2(req Request, generation uint64) bool {
	s.expireDataHandles()
	check := func() error { return s.check(req.ID, generation) }
	if req.Op >= OpComplete && req.Op <= OpStatus {
		if req.Op == OpCancel && req.Session == 0 {
			if s.pendingName != nil && s.pendingName.request == req.ID {
				s.pendingName = nil
			}
			if s.resolveReplay != nil && s.resolveReplay.request == req.ID {
				s.resolveReplay = nil
			}
			if s.listState != nil && s.listState.request == req.ID {
				s.clearListing()
			}
		}
		return false
	}
	if err := check(); err != nil {
		s.fail(req, codeFor(err))
		return true
	}
	if s.dataRoot == nil {
		s.fail(req, ErrDenied)
		return true
	}
	switch req.Op {
	case OpOpenRoot:
		id, err := s.addDirectory(".")
		if err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		s.send(req, append([]byte{RespOpenRoot, OK}, id[:]...))
	case OpListDir:
		d, ok := s.dataDirs[req.DirectoryID]
		if !ok {
			s.fail(req, ErrSession)
			return true
		}
		d.touched = time.Now()
		info, err := s.checkedInfo(d.path)
		if err != nil || !info.IsDir() || !sameDirectoryObject(identityOf(info), d.identity) {
			if s.listState != nil && s.listState.dir == req.DirectoryID {
				s.clearListing()
			}
			s.fail(req, ErrChanged)
			return true
		}
		current := identityOf(info)
		state := s.listState
		fresh := req.Cursor == 0 && req.NameOffset == 0 && (state == nil || state.dir != req.DirectoryID || state.cursor != 0 || state.request != req.ID)
		if fresh {
			s.clearListing()
			state = &directoryListing{dir: req.DirectoryID, snapshot: current, request: req.ID, touched: time.Now()}
			s.listState = state
		} else {
			if state == nil || state.dir != req.DirectoryID {
				s.fail(req, ErrChanged)
				return true
			}
			if state.snapshot != current {
				s.clearListing()
				s.fail(req, ErrChanged)
				return true
			}
			switch {
			case req.NameOffset > 0:
				if state.cursor != req.Cursor || state.request != req.ID || state.name == "" || state.end {
					s.fail(req, ErrChanged)
					return true
				}
			case req.Cursor == state.cursor:
				state.request = req.ID
			case req.Cursor == state.cursor+1:
				state.cursor = req.Cursor
				state.name = ""
				state.end = false
				state.request = req.ID
			default:
				s.fail(req, ErrChanged)
				return true
			}
		}
		state.touched = time.Now()
		directoryCheck := func() error {
			if err := check(); err != nil {
				return err
			}
			latest, err := s.checkedInfo(d.path)
			if err != nil || !latest.IsDir() || !sameDirectoryObject(identityOf(latest), d.identity) || identityOf(latest) != state.snapshot {
				return errChanged
			}
			return nil
		}
		entry, err := s.listEntry(d, req.Cursor, directoryCheck)
		if err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		if entry == nil {
			state.end = true
			state.request = req.ID
			body := []byte{RespListDir, OK}
			body = binary.LittleEndian.AppendUint32(body, req.Cursor)
			body = append(body, 0)
			s.send(req, body)
			return true
		}
		if req.NameOffset == 0 {
			state.request = req.ID
		}
		if int(req.NameOffset) >= len(entry.name) {
			s.fail(req, ErrInvalid)
			return true
		}
		end := min(len(entry.name), int(req.NameOffset)+listChunkSize)
		body := []byte{RespListDir, OK}
		body = binary.LittleEndian.AppendUint32(body, req.Cursor+1)
		body = append(body, entry.kind)
		body = binary.LittleEndian.AppendUint64(body, entry.size)
		body = binary.LittleEndian.AppendUint64(body, uint64(entry.modified))
		body = binary.LittleEndian.AppendUint16(body, uint16(len(entry.name)))
		body = binary.LittleEndian.AppendUint16(body, req.NameOffset)
		body = append(body, byte(end-int(req.NameOffset)))
		body = append(body, entry.name[int(req.NameOffset):end]...)
		s.send(req, body)
	case OpResolveName:
		s.resolveName(req)
	case OpOpenDir:
		n, ok := s.dataNodes[req.NodeID]
		if !ok {
			s.fail(req, ErrSession)
			return true
		}
		n.touched = time.Now()
		if n.kind != 2 {
			s.fail(req, ErrDenied)
			return true
		}
		info, err := s.validateNode(n)
		if err != nil || !info.IsDir() {
			s.fail(req, ErrChanged)
			return true
		}
		id, err := s.addDirectory(n.path)
		if err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		s.send(req, append([]byte{RespOpenDir, OK}, id[:]...))
	case OpCloseDir:
		if _, ok := s.dataDirs[req.DirectoryID]; !ok {
			s.fail(req, ErrSession)
			return true
		}
		delete(s.dataDirs, req.DirectoryID)
		if s.listState != nil && s.listState.dir == req.DirectoryID {
			s.clearListing()
		}
		s.send(req, []byte{RespCloseDir, OK})
	case OpStatNode:
		n, ok := s.dataNodes[req.NodeID]
		if !ok {
			s.fail(req, ErrSession)
			return true
		}
		n.touched = time.Now()
		if n.kind != 1 {
			s.fail(req, ErrDenied)
			return true
		}
		if err := s.active(true); err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		current, statErr := s.validateNode(n)
		if statErr != nil || current == nil || !current.Mode().IsRegular() {
			_ = s.active(false)
			s.fail(req, ErrChanged)
			return true
		}
		nodeCheck := func() error {
			if err := check(); err != nil {
				return err
			}
			_, err := s.validateNode(n)
			return err
		}
		f, err := s.openDataRead(n.path, nodeCheck)
		if err != nil {
			_ = s.active(false)
			s.fail(req, codeFor(err))
			return true
		}
		body := []byte{RespStatNode, OK}
		body = binary.LittleEndian.AppendUint64(body, f.info.Size)
		body = binary.LittleEndian.AppendUint64(body, uint64(f.info.Modified))
		body = append(body, f.info.Hash[:]...)
		f.close()
		_ = s.active(false)
		s.send(req, body)
	case OpMkdir:
		n, ok := s.dataNodes[req.NodeID]
		if !ok {
			s.fail(req, ErrSession)
			return true
		}
		n.touched = time.Now()
		if n.kind != 0 {
			s.fail(req, ErrChanged)
			return true
		}
		if err := s.active(true); err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		_, err := s.validateNode(n)
		info, parentErr := s.checkedInfo(filepath.Dir(n.path))
		if err == nil && parentErr == nil && info.IsDir() && sameDirectoryObject(identityOf(info), n.parentIdentity) {
			err = s.dataRoot.Mkdir(n.path, 0700)
		} else if err == nil {
			err = os.ErrPermission
		}
		_ = s.active(false)
		if err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		id, err := s.addDirectory(n.path)
		if err != nil {
			s.fail(req, codeFor(err))
			return true
		}
		s.send(req, append([]byte{RespMkdir, OK}, id[:]...))
	case OpGetNode, OpPutNode:
		s.startDataTransfer(req, generation)
	default:
		s.fail(req, ErrInvalid)
	}
	return true
}

type listedEntry struct {
	name     string
	kind     byte
	size     uint64
	modified int64
}

func (s *Server) listEntry(d *dataDirectory, cursor uint32, check func() error) (*listedEntry, error) {
	state := s.listState
	if state == nil || state.dir != findDirectoryID(s.dataDirs, d) {
		return nil, errChanged
	}
	if state.cursor == cursor && state.end {
		if err := check(); err != nil {
			s.clearListing()
			return nil, err
		}
		return nil, nil
	}
	if state.cursor == cursor && state.name != "" {
		if err := check(); err != nil {
			s.clearListing()
			return nil, err
		}
		state.touched = time.Now()
		return &listedEntry{name: state.name, kind: state.kind, size: state.size, modified: state.modified}, nil
	}
	if cursor != state.position {
		s.clearListing()
		return nil, errChanged
	}
	if state.iterator == nil {
		iterator, err := s.dataRoot.Open(d.path)
		if err != nil {
			s.clearListing()
			return nil, err
		}
		openedInfo, err := iterator.Stat()
		if err != nil || identityOf(openedInfo) != state.snapshot {
			iterator.Close()
			s.clearListing()
			return nil, errChanged
		}
		state.iterator = iterator
	}
	for {
		if err := check(); err != nil {
			s.clearListing()
			return nil, err
		}
		entries, err := state.iterator.ReadDir(1)
		if len(entries) == 0 {
			if err != nil && err != io.EOF {
				s.clearListing()
				return nil, err
			}
			if err := check(); err != nil {
				s.clearListing()
				return nil, err
			}
			_ = state.iterator.Close()
			state.iterator = nil
			state.end = true
			return nil, nil
		}
		e := entries[0]
		if e.Name() == ".partial" || e.Name() == ".ble-transfer" || strings.HasPrefix(e.Name(), ".ble-transfer-") || !utf8.ValidString(e.Name()) {
			continue
		}
		name := e.Name()
		kind := byte(3)
		size := uint64(0)
		modified := int64(0)
		if validComponent(name) {
			info, infoErr := e.Info()
			if infoErr == nil {
				modified = info.ModTime().Unix()
				if info.Mode().IsRegular() && info.Size() >= 0 && uint64(info.Size()) <= maxFileSize {
					kind = 1
					size = uint64(info.Size())
				} else if info.IsDir() {
					kind = 2
				}
			}
		}
		if err := check(); err != nil {
			s.clearListing()
			return nil, err
		}
		state.name = name
		state.kind = kind
		state.size = size
		state.modified = modified
		state.position++
		state.end = false
		state.touched = time.Now()
		return &listedEntry{name: name, kind: kind, size: size, modified: modified}, nil
	}
}
func findDirectoryID(ds map[[16]byte]*dataDirectory, d *dataDirectory) [16]byte {
	for id, v := range ds {
		if v == d {
			return id
		}
	}
	return [16]byte{}
}

func (s *Server) resolveName(req Request) {
	d, ok := s.dataDirs[req.DirectoryID]
	if !ok {
		s.fail(req, ErrSession)
		return
	}
	d.touched = time.Now()
	if replay := s.resolveReplay; s.pendingName == nil && replay != nil && replay.request == req.ID && replay.dir == req.DirectoryID && replay.total == req.NameTotal && replay.finalOffset == req.NameOffset && replay.finalChunk == req.Name && time.Since(replay.touched) <= 30*time.Second {
		info, err := s.checkedInfo(d.path)
		if err != nil || !info.IsDir() || identityOf(info) != d.identity {
			s.fail(req, ErrChanged)
			return
		}
		replay.touched = time.Now()
		s.send(req, replay.body)
		return
	}
	p := s.pendingName
	if p == nil || p.dir != req.DirectoryID || p.total != req.NameTotal || p.request != req.ID || time.Since(p.touched) > 30*time.Second {
		if req.NameOffset == 0 {
			s.resolveReplay = nil
		}
		if req.NameOffset != 0 {
			s.fail(req, ErrChanged)
			return
		}
		p = &pendingResolve{dir: req.DirectoryID, total: req.NameTotal, data: make([]byte, req.NameTotal), request: req.ID, touched: time.Now()}
		s.pendingName = p
	}
	if req.NameOffset > p.next || len(req.Name) == 0 || int(req.NameOffset)+len(req.Name) > int(p.total) {
		s.fail(req, ErrInvalid)
		return
	}
	if req.NameOffset < p.next {
		end := int(req.NameOffset) + len(req.Name)
		if end > int(p.next) || !bytes.Equal(p.data[int(req.NameOffset):end], []byte(req.Name)) {
			s.fail(req, ErrInvalid)
			return
		}
	} else {
		copy(p.data[int(req.NameOffset):int(req.NameOffset)+len(req.Name)], req.Name)
		p.next += uint16(len(req.Name))
	}
	p.touched = time.Now()
	next := p.next
	done := next == p.total
	body := []byte{RespResolveName, OK}
	body = binary.LittleEndian.AppendUint16(body, next)
	if done {
		name := string(p.data)
		s.pendingName = nil
		if !validComponent(name) {
			s.fail(req, ErrInvalid)
			return
		}
		parentInfo, parentErr := s.checkedInfo(d.path)
		if parentErr != nil || !parentInfo.IsDir() || !sameDirectoryObject(identityOf(parentInfo), d.identity) {
			s.fail(req, ErrChanged)
			return
		}
		path := name
		if d.path != "." {
			path = d.path + "/" + name
		}
		info, err := s.checkedInfo(path)
		var kind byte
		if errors.Is(err, os.ErrNotExist) {
			kind = 0
			info = nil
		} else if err != nil {
			s.fail(req, codeFor(err))
			return
		} else if info.Mode().IsRegular() && info.Size() >= 0 && uint64(info.Size()) <= maxFileSize {
			kind = 1
		} else if info.IsDir() {
			kind = 2
		} else {
			s.fail(req, ErrDenied)
			return
		}
		id, err := s.addNode(path, kind, info)
		if err != nil {
			s.fail(req, codeFor(err))
			return
		}
		node := s.dataNodes[id]
		node.parentPath = d.path
		node.parentIdentity = d.identity
		body = append(body, 1)
		body = appendID(body, id)
		body = append(body, kind)
		s.resolveReplay = &resolveReplay{dir: req.DirectoryID, request: req.ID, total: req.NameTotal, finalOffset: req.NameOffset, finalChunk: req.Name, body: append([]byte(nil), body...), nodeID: id, touched: time.Now()}
	} else {
		body = append(body, 0)
	}
	s.send(req, body)
}
func (s *Server) openDataRead(path string, check func() error) (*storedFile, error) {
	info, err := s.checkedInfo(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || uint64(info.Size()) > maxFileSize {
		return nil, os.ErrPermission
	}
	f, err := s.dataRoot.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || identityOf(opened) != identityOf(info) {
		f.Close()
		return nil, errChanged
	}
	result := &storedFile{root: s.dataRoot, borrowedRoot: true, file: f, check: check, info: FileInfo{Name: filepath.Base(path), Size: uint64(opened.Size()), Modified: opened.ModTime().Unix()}}
	hash, err := digest(f, result.info.Size, check)
	if err != nil {
		result.file = nil
		f.Close()
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || identityOf(after) != identityOf(opened) {
		f.Close()
		return nil, errChanged
	}
	result.info.Hash = hash
	return result, nil
}

func (s *Server) closeDataRoot() {
	s.clearListing()
	if s.dataRoot != nil {
		s.dataRoot.Close()
		s.dataRoot = nil
	}
}

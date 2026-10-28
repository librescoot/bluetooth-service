package filetransfer

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Writer interface{ WriteWithFrameID(byte, []byte) error }
type Hooks struct{ Active func(bool) error }
type packet struct {
	frame      byte
	data       []byte
	generation uint64
}
type transfer struct {
	request              Request
	id                   uint32
	stored               *storedFile
	offset, sent, synced uint64
	ackBase              uint64
	last                 time.Time
	retries              int
	rewindPending        bool
}

type Server struct {
	writer           func() Writer
	stores           map[byte]Store
	hooks            Hooks
	queue            chan packet
	reset            chan struct{}
	stop             chan struct{}
	done             chan struct{}
	once             sync.Once
	generation       atomic.Uint64
	busy             atomic.Bool
	cancelled        atomic.Uint32
	session          *transfer
	sequence         uint32
	catalog          []FileInfo
	catalogID        uint32
	catalogStore     byte
	catalogAt        time.Time
	completedID      uint32
	completedRequest uint32
	completedAt      time.Time
	idSource         io.Reader
	dataRoot         *os.Root
	dataPath         string
	dataDirs         map[[16]byte]*dataDirectory
	dataNodes        map[[16]byte]*dataNode
	pendingName      *pendingResolve
	resolveReplay    *resolveReplay
	listState        *directoryListing
}

func New(writer func() Writer, stores map[byte]Store, hooks Hooks) *Server {
	ownedStores := make(map[byte]Store, len(stores))
	for id, store := range stores {
		ownedStores[id] = store
	}
	s := &Server{writer: writer, stores: ownedStores, hooks: hooks, queue: make(chan packet, 128), reset: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), idSource: rand.Reader}
	go s.run()
	return s
}
func (s *Server) enqueue(frame byte, p []byte) {
	limit := MaxPayload
	if frame == FrameControl {
		limit = 128
	}
	if len(p) > limit {
		return
	}
	copyData := append([]byte(nil), p...)
	select {
	case s.queue <- packet{frame, copyData, s.generation.Load()}:
	case <-s.stop:
	default:
	}
}
func (s *Server) HandleData(p []byte) { s.enqueue(FrameData, p) }
func (s *Server) HandleControl(p []byte) {
	if req, err := DecodeRequest(p); err == nil && req.Op == OpCancel && req.Session == 0 {
		s.cancelled.Store(req.ID)
	}
	s.enqueue(FrameControl, p)
}
func (s *Server) Disconnect() {
	s.generation.Add(1)
	s.cancelled.Store(0)
	select {
	case s.reset <- struct{}{}:
	default:
	}
}
func (s *Server) Close()     { s.once.Do(func() { close(s.stop) }); <-s.done }
func (s *Server) Busy() bool { return s.busy.Load() }
func (s *Server) active(value bool) error {
	if value {
		s.busy.Store(true)
	}
	if s.hooks.Active != nil {
		if err := s.hooks.Active(value); err != nil {
			s.busy.Store(false)
			return err
		}
	}
	s.busy.Store(value)
	return nil
}
func (s *Server) finish() {
	if s.session != nil {
		s.session.stored.close()
		s.session = nil
		_ = s.active(false)
	}
}
func (s *Server) run() {
	defer close(s.done)
	defer s.finish()
	defer s.closeDataRoot()
	defer s.resetDataHandles()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	generation := s.generation.Load()
	for {
		select {
		case <-s.stop:
			return
		case <-s.reset:
			s.finish()
			s.catalog = nil
			s.resetDataHandles()
			generation = s.generation.Load()
		case p := <-s.queue:
			if generation != s.generation.Load() {
				s.finish()
				s.catalog = nil
				s.resetDataHandles()
				generation = s.generation.Load()
			}
			if p.generation != generation {
				continue
			}
			if p.frame == FrameData {
				s.data(p.data)
			} else {
				s.control(p.data, p.generation)
			}
		case now := <-ticker.C:
			if generation != s.generation.Load() {
				s.finish()
				s.resetDataHandles()
				generation = s.generation.Load()
			}
			t := s.session
			if t == nil {
				continue
			}
			if now.Sub(t.last) > 30*time.Second {
				s.finish()
				continue
			}
			if isDownload(t.request.Op) && t.sent > t.offset && now.Sub(t.last) > 2*time.Second {
				if t.retries >= 10 {
					s.fail(t.request, ErrIO)
					s.finish()
					continue
				}
				t.sent = t.offset
				t.retries++
				t.last = now
				s.pump()
			}
		}
	}
}
func (s *Server) send(req Request, body []byte) {
	s.sequence++
	fragments, err := Fragments(req.ID, s.sequence, req.Budget, body)
	if err != nil {
		return
	}
	w := s.writer()
	if w == nil {
		return
	}
	for _, p := range fragments {
		if w.WriteWithFrameID(FrameStatus, p) != nil {
			return
		}
	}
}
func (s *Server) fail(req Request, code byte) { s.send(req, []byte{RespError, code}) }
func isDownload(op byte) bool                 { return op == OpGet || op == OpGetNode }
func codeFor(err error) byte {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ErrNotFound
	case errors.Is(err, errCancelled):
		return ErrSession
	case errors.Is(err, errChecksum):
		return ErrIntegrity
	case errors.Is(err, errChanged), errors.Is(err, os.ErrExist):
		return ErrChanged
	case errors.Is(err, syscall.ENAMETOOLONG):
		return ErrInvalid
	case errors.Is(err, ErrTransportBusy):
		return ErrBusy
	case errors.Is(err, os.ErrPermission):
		return ErrDenied
	case errors.Is(err, syscall.ENOSPC):
		return ErrSpace
	default:
		return ErrIO
	}
}
func (s *Server) startAck(t *transfer) {
	p := []byte{RespStart, OK}
	p = binary.LittleEndian.AppendUint32(p, t.id)
	p = binary.LittleEndian.AppendUint64(p, t.offset)
	p = binary.LittleEndian.AppendUint64(p, t.stored.info.Size)
	p = binary.LittleEndian.AppendUint16(p, t.request.Chunk)
	p = binary.LittleEndian.AppendUint16(p, Window)
	p = append(p, t.stored.info.Hash[:]...)
	s.send(t.request, p)
}
func (s *Server) ack(t *transfer, rewind bool) {
	p := []byte{RespAck}
	p = binary.LittleEndian.AppendUint32(p, t.id)
	p = binary.LittleEndian.AppendUint64(p, t.offset)
	if rewind {
		p = append(p, 1)
	} else {
		p = append(p, 0)
	}
	s.send(t.request, p)
}
func (s *Server) check(id uint32, generation uint64) error {
	select {
	case <-s.stop:
		return errCancelled
	default:
	}
	if s.generation.Load() != generation || s.cancelled.Load() == id {
		return errCancelled
	}
	return nil
}
func (s *Server) control(p []byte, generation uint64) {
	req, err := DecodeRequest(p)
	if err != nil {
		if len(p) >= 8 && p[1] == Version2 {
			id := binary.LittleEndian.Uint32(p[2:6])
			budget := binary.LittleEndian.Uint16(p[6:8])
			if id != 0 && budget >= 20 && budget <= MaxPayload {
				s.fail(Request{Version: Version2, ID: id, Budget: budget}, ErrInvalid)
			}
		}
		return
	}
	check := func() error { return s.check(req.ID, generation) }
	if req.Version == Version2 && s.controlV2(req, generation) {
		return
	}
	if req.Op == OpList || req.Op == OpStat || req.Op == OpPut || req.Op == OpGet {
		if err := check(); err != nil {
			s.fail(req, codeFor(err))
			return
		}
	}
	if req.Op == OpCancel && req.Session == 0 && (s.session == nil || s.session.request.ID != req.ID) {
		s.send(req, []byte{RespCancel, OK})
		return
	}
	switch req.Op {
	case OpList:
		store, ok := s.stores[req.Store]
		if !ok {
			s.fail(req, ErrDenied)
			return
		}
		if req.Index == 0 || req.ID != s.catalogID || req.Store != s.catalogStore || time.Since(s.catalogAt) > time.Minute {
			if req.Index != 0 {
				s.fail(req, ErrChanged)
				return
			}
			files, err := store.List()
			if err != nil {
				s.fail(req, codeFor(err))
				return
			}
			s.catalog = files
			s.catalogID = req.ID
			s.catalogStore = req.Store
			s.catalogAt = time.Now()
		}
		if int(req.Index) >= len(s.catalog) {
			s.send(req, []byte{RespList, 1})
			return
		}
		info := s.catalog[req.Index]
		body := []byte{RespList, OK}
		body = binary.LittleEndian.AppendUint32(body, req.Index+1)
		body = binary.LittleEndian.AppendUint64(body, info.Size)
		body = binary.LittleEndian.AppendUint64(body, uint64(info.Modified))
		body = append(body, byte(len(info.Name)))
		body = append(body, info.Name...)
		s.send(req, body)
	case OpStat:
		store, ok := s.stores[req.Store]
		if !ok {
			s.fail(req, ErrDenied)
			return
		}
		temporary := s.session == nil
		if temporary {
			if err := s.active(true); err != nil {
				s.fail(req, codeFor(err))
				return
			}
			defer s.active(false)
		}
		file, err := store.read(req.Name, check)
		if err != nil {
			s.fail(req, codeFor(err))
			return
		}
		defer file.close()
		body := []byte{RespStat, OK}
		body = binary.LittleEndian.AppendUint64(body, file.info.Size)
		body = append(body, file.info.Hash[:]...)
		s.send(req, body)
	case OpPut, OpGet:
		if s.session != nil {
			if s.session.request == req {
				s.startAck(s.session)
				return
			}
			s.fail(req, ErrBusy)
			return
		}
		store, ok := s.stores[req.Store]
		if !ok {
			s.fail(req, ErrDenied)
			return
		}
		if err := s.active(true); err != nil {
			s.fail(req, codeFor(err))
			return
		}
		var file *storedFile
		var offset uint64
		if req.Op == OpPut {
			file, offset, err = store.write(req, check)
		} else {
			file, err = store.read(req.Name, check)
			offset = req.Offset
			if err == nil && (file.info.Hash != req.Hash || offset > file.info.Size || offset%uint64(req.Chunk) != 0) {
				file.close()
				file = nil
				s.fail(req, ErrChanged)
				_ = s.active(false)
				return
			}
		}
		if err != nil {
			s.fail(req, codeFor(err))
			_ = s.active(false)
			return
		}
		var nonce [4]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			file.close()
			s.fail(req, ErrIO)
			_ = s.active(false)
			return
		}
		id := binary.LittleEndian.Uint32(nonce[:])
		if id == 0 {
			id = 1
		}
		s.session = &transfer{request: req, id: id, stored: file, offset: offset, sent: offset, synced: offset, last: time.Now()}
		s.startAck(s.session)
		if isDownload(req.Op) {
			s.pump()
		}
	case OpComplete, OpCancel, OpAck, OpStatus:
		t := s.session
		if t == nil || (req.Session != t.id && !(req.Op == OpCancel && req.Session == 0)) {
			if req.Op == OpComplete && req.Session == s.completedID && req.ID == s.completedRequest && time.Since(s.completedAt) < time.Minute {
				s.send(req, []byte{RespComplete, OK})
				return
			}
			s.fail(req, ErrSession)
			return
		}
		if req.ID != t.request.ID {
			s.fail(req, ErrSession)
			return
		}
		t.last = time.Now()
		t.retries = 0
		switch req.Op {
		case OpCancel:
			s.send(req, []byte{RespCancel, OK})
			s.finish()
		case OpStatus:
			s.ack(t, false)
		case OpAck:
			if !isDownload(t.request.Op) || req.Offset < t.offset || req.Offset > t.sent || (req.Offset != t.stored.info.Size && req.Offset%uint64(t.request.Chunk) != 0) {
				s.fail(req, ErrInvalid)
				return
			}
			t.offset = req.Offset
			if req.Rewind {
				t.sent = req.Offset
			}
			s.pump()
		case OpComplete:
			if t.offset != t.stored.info.Size {
				s.fail(req, ErrInvalid)
				return
			}
			if !isDownload(t.request.Op) {
				if err := t.stored.commit(); err != nil {
					s.fail(req, codeFor(err))
					if errors.Is(err, errChecksum) {
						if t.stored.isData {
							_ = t.stored.root.Remove(t.stored.stagePath)
						} else {
							_ = t.stored.root.Remove(t.stored.partial)
							_ = t.stored.root.Remove(".partial/" + t.stored.info.Name + ".json")
						}
					}
					s.finish()
					return
				}
			}
			s.completedID = t.id
			s.completedRequest = req.ID
			s.completedAt = time.Now()
			s.send(req, []byte{RespComplete, OK})
			s.finish()
		}
	}
}
func (s *Server) data(p []byte) {
	t := s.session
	if t == nil || isDownload(t.request.Op) {
		return
	}
	if t.stored.check != nil {
		if err := t.stored.check(); err != nil {
			s.fail(t.request, codeFor(err))
			s.finish()
			return
		}
	}
	id, offset, data, err := DecodeData(p)
	if err != nil || id != t.id {
		return
	}
	if offset != t.offset {
		if offset < t.offset {
			s.ack(t, false)
		} else if !t.rewindPending {
			t.rewindPending = true
			s.ack(t, true)
		}
		return
	}
	t.rewindPending = false
	remaining := t.stored.info.Size - t.offset
	if uint64(len(data)) > remaining || len(data) != int(min(uint64(t.request.Chunk), remaining)) {
		s.fail(t.request, ErrInvalid)
		return
	}
	if _, err := t.stored.file.WriteAt(data, int64(offset)); err != nil {
		s.fail(t.request, ErrIO)
		s.finish()
		return
	}
	t.offset += uint64(len(data))
	t.last = time.Now()
	if t.offset-t.synced >= 256<<10 {
		if t.stored.file.Sync() != nil {
			s.fail(t.request, ErrIO)
			s.finish()
			return
		}
		t.synced = t.offset
	}
	if t.offset == t.stored.info.Size || t.offset-t.ackBase >= uint64(Window/2)*uint64(t.request.Chunk) {
		t.ackBase = t.offset
		s.ack(t, false)
	}
}
func (s *Server) pump() {
	t := s.session
	if t == nil || !isDownload(t.request.Op) {
		return
	}
	if t.stored.check != nil {
		if err := t.stored.check(); err != nil {
			s.fail(t.request, codeFor(err))
			s.finish()
			return
		}
	}
	w := s.writer()
	if w == nil {
		return
	}
	limit := min(t.stored.info.Size, t.offset+uint64(Window)*uint64(t.request.Chunk))
	for t.sent < limit {
		if t.stored.check != nil {
			if err := t.stored.check(); err != nil {
				s.fail(t.request, codeFor(err))
				s.finish()
				return
			}
		}
		size := min(uint64(t.request.Chunk), t.stored.info.Size-t.sent)
		data := make([]byte, size)
		n, err := t.stored.file.ReadAt(data, int64(t.sent))
		if n != len(data) || (err != nil && err != io.EOF) {
			s.fail(t.request, ErrChanged)
			s.finish()
			return
		}
		if w.WriteWithFrameID(FrameData, EncodeData(t.id, t.sent, data)) != nil {
			return
		}
		t.sent += size
	}
}

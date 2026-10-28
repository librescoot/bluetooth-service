package filetransfer

import (
	"crypto/rand"
	"encoding/binary"
	"os"
	"time"
)

func (s *Server) startDataTransfer(req Request, generation uint64) {
	if s.session != nil {
		if s.session.request == req {
			s.startAck(s.session)
			return
		}
		s.fail(req, ErrBusy)
		return
	}
	n, ok := s.dataNodes[req.NodeID]
	if !ok {
		s.fail(req, ErrSession)
		return
	}
	if err := s.active(true); err != nil {
		s.fail(req, codeFor(err))
		return
	}
	n.touched = time.Now()
	check := func() error {
		if err := s.check(req.ID, generation); err != nil {
			return err
		}
		_, err := s.validateNode(n)
		return err
	}
	var f *storedFile
	var offset uint64
	var err error
	if req.Op == OpGetNode {
		if n.kind != 1 {
			err = os.ErrPermission
		} else {
			info, e := s.validateNode(n)
			if e != nil || info == nil || !info.Mode().IsRegular() {
				err = errChanged
			} else {
				f, err = s.openDataRead(n.path, check)
				if err == nil && (f.info.Hash != req.Hash || req.Offset > f.info.Size || req.Offset%uint64(req.Chunk) != 0) {
					f.close()
					f = nil
					err = errChanged
				}
				if err == nil {
					offset = req.Offset
				}
			}
		}
	} else {
		if n.kind == 0 {
			if req.Intent != IntentCreate {
				err = errChanged
			} else if _, e := s.validateNode(n); e != nil {
				err = e
			}
		} else if n.kind == 1 {
			if req.Intent != IntentReplace {
				err = os.ErrExist
			} else if info, e := s.validateNode(n); e != nil || info == nil || !info.Mode().IsRegular() {
				err = errChanged
			}
		} else {
			err = os.ErrPermission
		}
		if err == nil {
			f, offset, err = s.beginDataWrite(req, n.path, check)
			if f != nil {
				f.root = s.dataRoot
				f.borrowedRoot = true
			}
		}
	}
	if err != nil {
		if f != nil {
			f.close()
		}
		_ = s.active(false)
		s.fail(req, codeFor(err))
		return
	}
	var nonce [4]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		f.close()
		_ = s.active(false)
		s.fail(req, ErrIO)
		return
	}
	id := binary.LittleEndian.Uint32(nonce[:])
	if id == 0 {
		id = 1
	}
	s.session = &transfer{request: req, id: id, stored: f, offset: offset, sent: offset, synced: offset, last: time.Now()}
	s.startAck(s.session)
	if req.Op == OpGetNode {
		s.pump()
	}
}

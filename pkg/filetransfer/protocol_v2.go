package filetransfer

import (
	"encoding/binary"
	"strings"
	"unicode/utf8"
)

func decodeV2(r *Request, p []byte) error {
	need := func(n int) error {
		if len(p) != n {
			return errMalformed
		}
		return nil
	}
	copyID := func(dst *[16]byte) { copy(dst[:], p) }
	switch r.Op {
	case OpOpenRoot:
		return need(0)
	case OpListDir:
		if len(p) != 22 {
			return errMalformed
		}
		copyID(&r.DirectoryID)
		r.Cursor = binary.LittleEndian.Uint32(p[16:20])
		r.NameOffset = binary.LittleEndian.Uint16(p[20:22])
	case OpOpenDir, OpStatNode, OpMkdir, OpCloseDir:
		if err := need(16); err != nil {
			return err
		}
		if r.Op == OpCloseDir {
			copyID(&r.DirectoryID)
		} else {
			copyID(&r.NodeID)
		}
	case OpResolveName:
		if len(p) < 20 || len(p) > 120 {
			return errMalformed
		}
		copyID(&r.DirectoryID)
		r.NameOffset = binary.LittleEndian.Uint16(p[16:18])
		r.NameTotal = binary.LittleEndian.Uint16(p[18:20])
		if r.NameTotal == 0 || r.NameTotal > 255 || int(r.NameOffset)+len(p)-20 > int(r.NameTotal) {
			return errMalformed
		}
		r.Name = string(p[20:])
	case OpGetNode:
		if err := need(58); err != nil {
			return err
		}
		copyID(&r.NodeID)
		r.Offset = binary.LittleEndian.Uint64(p[16:24])
		r.Chunk = binary.LittleEndian.Uint16(p[24:26])
		copy(r.Hash[:], p[26:58])
		if r.Chunk == 0 || int(r.Chunk) > int(r.Budget)-DataHeader {
			return errMalformed
		}
	case OpPutNode:
		if err := need(99); err != nil {
			return err
		}
		copyID(&r.NodeID)
		r.Intent = p[16]
		r.Chunk = binary.LittleEndian.Uint16(p[17:19])
		r.Size = binary.LittleEndian.Uint64(p[19:27])
		copy(r.Hash[:], p[27:59])
		r.OldSize = binary.LittleEndian.Uint64(p[59:67])
		copy(r.OldHash[:], p[67:99])
		if r.Intent > IntentReplace || r.Chunk == 0 || int(r.Chunk) > int(r.Budget)-DataHeader || (r.Intent == IntentCreate && (r.OldSize != 0 || r.OldHash != [32]byte{})) {
			return errMalformed
		}
	default:
		if r.Op >= OpComplete && r.Op <= OpStatus {
			return decodeV1Body(r, p)
		}
		return errMalformed
	}
	return nil
}

func decodeV1Body(r *Request, p []byte) error {
	switch r.Op {
	case OpComplete, OpCancel, OpStatus:
		if len(p) != 4 {
			return errMalformed
		}
		r.Session = binary.LittleEndian.Uint32(p)
	case OpAck:
		if len(p) != 13 || p[12] > 1 {
			return errMalformed
		}
		r.Session = binary.LittleEndian.Uint32(p)
		r.Offset = binary.LittleEndian.Uint64(p[4:12])
		r.Rewind = p[12] != 0
	default:
		return errMalformed
	}
	return nil
}

func encodeV2(r Request) []byte {
	p := []byte{r.Op, Version2}
	p = binary.LittleEndian.AppendUint32(p, r.ID)
	p = binary.LittleEndian.AppendUint16(p, r.Budget)
	appendID := func(id [16]byte) { p = append(p, id[:]...) }
	switch r.Op {
	case OpOpenRoot:
	case OpListDir:
		appendID(r.DirectoryID)
		p = binary.LittleEndian.AppendUint32(p, r.Cursor)
		p = binary.LittleEndian.AppendUint16(p, r.NameOffset)
	case OpOpenDir, OpMkdir:
		appendID(r.NodeID)
	case OpStatNode:
		appendID(r.NodeID)
	case OpCloseDir:
		appendID(r.DirectoryID)
	case OpResolveName:
		appendID(r.DirectoryID)
		p = binary.LittleEndian.AppendUint16(p, r.NameOffset)
		p = binary.LittleEndian.AppendUint16(p, r.NameTotal)
		p = append(p, r.Name...)
	case OpGetNode:
		appendID(r.NodeID)
		p = binary.LittleEndian.AppendUint64(p, r.Offset)
		p = binary.LittleEndian.AppendUint16(p, r.Chunk)
		p = append(p, r.Hash[:]...)
	case OpPutNode:
		appendID(r.NodeID)
		p = append(p, r.Intent)
		p = binary.LittleEndian.AppendUint16(p, r.Chunk)
		p = binary.LittleEndian.AppendUint64(p, r.Size)
		p = append(p, r.Hash[:]...)
		p = binary.LittleEndian.AppendUint64(p, r.OldSize)
		p = append(p, r.OldHash[:]...)
	case OpComplete, OpCancel, OpStatus:
		p = binary.LittleEndian.AppendUint32(p, r.Session)
	case OpAck:
		p = binary.LittleEndian.AppendUint32(p, r.Session)
		p = binary.LittleEndian.AppendUint64(p, r.Offset)
		if r.Rewind {
			p = append(p, 1)
		} else {
			p = append(p, 0)
		}
	}
	return p
}

func validComponent(name string) bool {
	if name == "" || len(name) > 255 || name == "." || name == ".." || name == ".partial" || strings.HasPrefix(name, ".ble-transfer") || !utf8.ValidString(name) {
		return false
	}
	for _, c := range name {
		if c == '/' || c == 0 {
			return false
		}
	}
	return true
}

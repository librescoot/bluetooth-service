// Package filetransfer implements a versioned, bidirectional binary file
// transport. Storage policy and file activation are separate from transfer.
package filetransfer

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Version               = 1
	FrameData        byte = 0xB3
	FrameControl     byte = 0xB4
	FrameStatus      byte = 0xB5
	MaxPayload            = 244
	MaxStatusPayload      = 128
	DataHeader            = 12
	StatusHeader          = 12
	Window                = 8
	OpList           byte = 1
	OpStat           byte = 2
	OpPut            byte = 3
	OpGet            byte = 4
	OpComplete       byte = 5
	OpCancel         byte = 6
	OpAck            byte = 7
	OpStatus         byte = 8
	RespList         byte = 0x81
	RespStat         byte = 0x82
	RespStart        byte = 0x83
	RespAck          byte = 0x84
	RespComplete     byte = 0x85
	RespCancel       byte = 0x86
	RespError        byte = 0x87
	StoreLogs        byte = 0
	StoreInbox       byte = 1
	OK               byte = 0
	ErrInvalid       byte = 1
	ErrNotFound      byte = 2
	ErrBusy          byte = 3
	ErrDenied        byte = 4
	ErrSpace         byte = 5
	ErrChanged       byte = 6
	ErrIntegrity     byte = 7
	ErrIO            byte = 8
	ErrSession       byte = 9
)

var errMalformed = errors.New("malformed file-transfer request")

type Request struct {
	Op           byte
	ID           uint32
	Budget       uint16
	Store        byte
	Name         string
	Index        uint32
	Size, Offset uint64
	Hash         [32]byte
	Chunk        uint16
	Session      uint32
	Rewind       bool
}

func ValidName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, c := range []byte(name) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '-') {
			continue
		}
		return false
	}
	return true
}

func DecodeRequest(p []byte) (Request, error) {
	var r Request
	if len(p) < 8 || p[1] != Version {
		return r, errMalformed
	}
	r.Op = p[0]
	r.ID = binary.LittleEndian.Uint32(p[2:6])
	r.Budget = binary.LittleEndian.Uint16(p[6:8])
	if r.ID == 0 || r.Budget < 20 || r.Budget > MaxPayload {
		return r, errMalformed
	}
	p = p[8:]
	switch r.Op {
	case OpList:
		if len(p) != 5 {
			return r, errMalformed
		}
		r.Store = p[0]
		r.Index = binary.LittleEndian.Uint32(p[1:])
	case OpStat, OpPut, OpGet:
		if len(p) < 2 {
			return r, errMalformed
		}
		r.Store = p[0]
		n := int(p[1])
		p = p[2:]
		if len(p) < n {
			return r, errMalformed
		}
		r.Name = string(p[:n])
		p = p[n:]
		if !ValidName(r.Name) {
			return r, errMalformed
		}
		if r.Op == OpStat {
			if len(p) != 0 {
				return r, errMalformed
			}
			break
		}
		if len(p) != 42 {
			return r, errMalformed
		}
		r.Chunk = binary.LittleEndian.Uint16(p)
		r.Size = binary.LittleEndian.Uint64(p[2:10])
		copy(r.Hash[:], p[10:42])
		if r.Op == OpGet {
			r.Offset = r.Size
			r.Size = 0
		}
		if r.Chunk == 0 || int(r.Chunk) > int(r.Budget)-DataHeader {
			return r, errMalformed
		}
	case OpComplete, OpCancel, OpStatus:
		if len(p) != 4 {
			return r, errMalformed
		}
		r.Session = binary.LittleEndian.Uint32(p)
	case OpAck:
		if len(p) != 13 || p[12] > 1 {
			return r, errMalformed
		}
		r.Session = binary.LittleEndian.Uint32(p)
		r.Offset = binary.LittleEndian.Uint64(p[4:12])
		r.Rewind = p[12] != 0
	default:
		return r, errMalformed
	}
	return r, nil
}

func EncodeRequest(r Request) []byte {
	p := []byte{r.Op, Version}
	p = binary.LittleEndian.AppendUint32(p, r.ID)
	p = binary.LittleEndian.AppendUint16(p, r.Budget)
	switch r.Op {
	case OpList:
		p = append(p, r.Store)
		p = binary.LittleEndian.AppendUint32(p, r.Index)
	case OpStat, OpPut, OpGet:
		p = append(p, r.Store, byte(len(r.Name)))
		p = append(p, r.Name...)
		if r.Op != OpStat {
			p = binary.LittleEndian.AppendUint16(p, r.Chunk)
			size := r.Size
			if r.Op == OpGet {
				size = r.Offset
			}
			p = binary.LittleEndian.AppendUint64(p, size)
			p = append(p, r.Hash[:]...)
		}
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

func EncodeData(session uint32, offset uint64, data []byte) []byte {
	p := binary.LittleEndian.AppendUint32(nil, session)
	p = binary.LittleEndian.AppendUint64(p, offset)
	return append(p, data...)
}
func DecodeData(p []byte) (uint32, uint64, []byte, error) {
	if len(p) <= DataHeader || len(p) > MaxPayload {
		return 0, 0, nil, errMalformed
	}
	return binary.LittleEndian.Uint32(p), binary.LittleEndian.Uint64(p[4:]), p[DataHeader:], nil
}

// Responses are fragmented to the negotiated ATT payload. Sequence IDs keep
// fragments from a superseded response from joining a newer message.
func Fragments(id, sequence uint32, budget uint16, body []byte) ([][]byte, error) {
	if budget < 20 || budget > MaxPayload || len(body) == 0 || len(body) > 256 {
		return nil, fmt.Errorf("invalid response size")
	}
	limit := min(int(budget), MaxStatusPayload) - StatusHeader
	var out [][]byte
	for offset := 0; offset < len(body); {
		end := min(len(body), offset+limit)
		p := binary.LittleEndian.AppendUint32(nil, id)
		p = binary.LittleEndian.AppendUint32(p, sequence)
		p = binary.LittleEndian.AppendUint16(p, uint16(offset))
		p = binary.LittleEndian.AppendUint16(p, uint16(len(body)))
		out = append(out, append(p, body[offset:end]...))
		offset = end
	}
	return out, nil
}

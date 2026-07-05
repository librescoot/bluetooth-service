// Package ota implements the scooter side of the BLE OTA firmware transfer:
// a windowed, resumable, integrity-checked receiver for update bundles pushed
// by the phone app through the nRF52's transparent OTA tunnel.
//
// Transport: the phone writes to the OTA GATT characteristics; the nRF forwards
// the values verbatim as raw USOCK frames (0xB0 data / 0xB1 control) and relays
// our status frames (0xB2) back as notifications. All multi-byte fields are
// little-endian. The protocol is mirrored in the app's ota_protocol.dart.
package ota

import (
	"encoding/binary"
	"fmt"
)

// Phone -> scooter control opcodes (OTA_CONTROL characteristic / frame 0xB1).
const (
	OpStart     = 0x01
	OpComplete  = 0x03
	OpAbort     = 0x04
	OpStatusReq = 0x05
)

// Scooter -> phone opcodes (OTA_STATUS characteristic / frame 0xB2).
const (
	OpStartAck        = 0x81
	OpAck             = 0x82
	OpCompleteAck     = 0x83
	OpInstallProgress = 0x84
	OpAbortAck        = 0x85
	OpError           = 0x86
)

// START_ACK status codes.
const (
	StartResume     = 0x00 // resuming an interrupted transfer at resume_offset
	StartNew        = 0x01 // fresh transfer from offset 0
	StartNoSpace    = 0x10 // not enough free staging space
	StartBusy       = 0x11 // another transfer session is active
	StartBadParams  = 0x12 // malformed START or unsupported component/chunk size
	StartInstalling = 0x13 // an install is in progress; poll with STATUS_REQ
)

// ACK flag bits.
const (
	AckFlagRewind = 0x01 // gap detected: resend from acked_offset (go-back-N)
)

// COMPLETE_ACK status codes.
const (
	CompleteOK           = 0x00 // hash verified, install queued
	CompleteSHAMismatch  = 0x01
	CompleteSizeMismatch = 0x02
	CompleteQueueFailed  = 0x03
)

// INSTALL_PROGRESS phases.
const (
	PhaseVerifying     = 0x00
	PhaseInstalling    = 0x01
	PhasePendingReboot = 0x02
	PhaseRebooting     = 0x03
	PhaseSuccess       = 0x04
	PhaseFailure       = 0x05
	PhaseIdle          = 0x06
)

// ERROR codes. 1 and 2 are emitted by the nRF tunnel itself (see ota_tunnel.c).
const (
	ErrSuspended   = 0x01 // iMX suspended, data dropped (nRF-originated)
	ErrOverflow    = 0x02 // nRF TX ring overflow, data dropped (nRF-originated)
	ErrInternal    = 0x03
	ErrWriteFailed = 0x04
	ErrNoSpace     = 0x05
	ErrNoSession   = 0x06
)

// ABORT reasons (phone -> scooter).
const (
	AbortUserCancel = 0x00
	AbortAppError   = 0x01
)

// Component identifiers in START.
const (
	ComponentMDB = 0x00
	ComponentDBC = 0x01
)

// MaxChunkSize is the largest data chunk the scooter accepts: a full ATT-MTU-247
// write minus the 3-byte ATT header and the 4-byte offset prefix.
const MaxChunkSize = 240

// Start is the decoded START control message.
type Start struct {
	Version   byte
	Component byte
	ChunkSize uint16
	TotalSize uint32
	SHA256    [32]byte
	BundleID  string
}

// DecodeStart parses a START payload:
// [op][ver][component][chunk_size:u16][total_size:u32][sha256:32][id_len][bundle_id].
func DecodeStart(p []byte) (Start, error) {
	var s Start
	if len(p) < 42 {
		return s, fmt.Errorf("START too short: %d bytes", len(p))
	}
	if p[0] != OpStart {
		return s, fmt.Errorf("not a START message: opcode 0x%02x", p[0])
	}
	s.Version = p[1]
	s.Component = p[2]
	s.ChunkSize = binary.LittleEndian.Uint16(p[3:5])
	s.TotalSize = binary.LittleEndian.Uint32(p[5:9])
	copy(s.SHA256[:], p[9:41])
	idLen := int(p[41])
	if idLen == 0 || idLen > 64 {
		return s, fmt.Errorf("invalid bundle id length %d", idLen)
	}
	if len(p) < 42+idLen {
		return s, fmt.Errorf("START truncated: id_len=%d but %d bytes left", idLen, len(p)-42)
	}
	s.BundleID = string(p[42 : 42+idLen])
	if !validBundleID(s.BundleID) {
		return s, fmt.Errorf("invalid bundle id %q", s.BundleID)
	}
	return s, nil
}

// validBundleID restricts bundle IDs to a filename-safe charset: the ID is
// joined verbatim into staging paths, so path separators and a leading dot
// (".", "..", hidden files) must never get through. The same rule is enforced
// by the app before sending. First char alphanumeric, rest [A-Za-z0-9._-].
func validBundleID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// EncodeStart builds a START payload (used by tests and bench tooling).
func EncodeStart(s Start) []byte {
	p := make([]byte, 0, 42+len(s.BundleID))
	p = append(p, OpStart, s.Version, s.Component)
	p = binary.LittleEndian.AppendUint16(p, s.ChunkSize)
	p = binary.LittleEndian.AppendUint32(p, s.TotalSize)
	p = append(p, s.SHA256[:]...)
	p = append(p, byte(len(s.BundleID)))
	p = append(p, s.BundleID...)
	return p
}

// DecodeData parses a DATA payload: [offset:u32][chunk].
func DecodeData(p []byte) (offset uint32, data []byte, err error) {
	if len(p) < 5 {
		return 0, nil, fmt.Errorf("DATA too short: %d bytes", len(p))
	}
	return binary.LittleEndian.Uint32(p[0:4]), p[4:], nil
}

// EncodeData builds a DATA payload (used by tests and bench tooling).
func EncodeData(offset uint32, data []byte) []byte {
	p := make([]byte, 0, 4+len(data))
	p = binary.LittleEndian.AppendUint32(p, offset)
	return append(p, data...)
}

// EncodeStartAck builds
// [op][status][resume_offset:u32][window_chunks:u16][ack_every][max_chunk:u16].
func EncodeStartAck(status byte, resumeOffset uint32, windowChunks uint16, ackEvery byte, maxChunk uint16) []byte {
	p := make([]byte, 0, 11)
	p = append(p, OpStartAck, status)
	p = binary.LittleEndian.AppendUint32(p, resumeOffset)
	p = binary.LittleEndian.AppendUint16(p, windowChunks)
	p = append(p, ackEvery)
	p = binary.LittleEndian.AppendUint16(p, maxChunk)
	return p
}

// EncodeAck builds [op][flags][acked_offset:u32].
func EncodeAck(flags byte, offset uint32) []byte {
	p := make([]byte, 0, 6)
	p = append(p, OpAck, flags)
	return binary.LittleEndian.AppendUint32(p, offset)
}

// EncodeCompleteAck builds [op][status].
func EncodeCompleteAck(status byte) []byte {
	return []byte{OpCompleteAck, status}
}

// EncodeInstallProgress builds [op][phase][percent][msg_len][msg]; msg is
// truncated to fit the 64-byte OTA_STATUS characteristic.
func EncodeInstallProgress(phase, percent byte, msg string) []byte {
	if len(msg) > 60 {
		msg = msg[:60]
	}
	p := make([]byte, 0, 4+len(msg))
	p = append(p, OpInstallProgress, phase, percent, byte(len(msg)))
	return append(p, msg...)
}

// EncodeError builds [op][code][msg_len][msg].
func EncodeError(code byte, msg string) []byte {
	if len(msg) > 60 {
		msg = msg[:60]
	}
	p := make([]byte, 0, 3+len(msg))
	p = append(p, OpError, code, byte(len(msg)))
	return append(p, msg...)
}

// EncodeAbortAck builds [op].
func EncodeAbortAck() []byte {
	return []byte{OpAbortAck}
}

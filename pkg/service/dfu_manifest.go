package service

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

// FwType mirrors the FwType enum from Nordic's dfu-cc.proto.
type FwType uint32

const (
	FwTypeApplication          FwType = 0
	FwTypeSoftDevice           FwType = 1
	FwTypeBootloader           FwType = 2
	FwTypeSoftDeviceBootloader FwType = 3
)

func (t FwType) String() string {
	switch t {
	case FwTypeApplication:
		return "APPLICATION"
	case FwTypeSoftDevice:
		return "SOFTDEVICE"
	case FwTypeBootloader:
		return "BOOTLOADER"
	case FwTypeSoftDeviceBootloader:
		return "SOFTDEVICE_BOOTLOADER"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint32(t))
	}
}

// InitPacketInfo is the subset of fields we care about from an init packet.
type InitPacketInfo struct {
	FwVersion uint32
	FwType    FwType
}

// ZipDFUInfo is the parsed view of a Nordic DFU zip's init packets, keyed by
// dat filename ("application.dat", "sd_bl.dat", etc.).
type ZipDFUInfo struct {
	Path        string
	InitPackets map[string]InitPacketInfo
}

// ApplicationInfo returns the application init packet if this zip contains one.
func (z *ZipDFUInfo) ApplicationInfo() (InitPacketInfo, bool) {
	for _, info := range z.InitPackets {
		if info.FwType == FwTypeApplication {
			return info, true
		}
	}
	return InitPacketInfo{}, false
}

// BootloaderInfo returns the bootloader+softdevice init packet if this zip
// contains one.
func (z *ZipDFUInfo) BootloaderInfo() (InitPacketInfo, bool) {
	for _, info := range z.InitPackets {
		if info.FwType == FwTypeSoftDeviceBootloader ||
			info.FwType == FwTypeBootloader {
			return info, true
		}
	}
	return InitPacketInfo{}, false
}

// ParseZipDFUInfo opens a Nordic DFU zip and extracts the fw_version / fw_type
// from every .dat init packet it contains. Works on -app-, -bl-, and -full-
// zips uniformly.
func ParseZipDFUInfo(path string) (*ZipDFUInfo, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("opening DFU zip %q: %w", path, err)
	}
	defer r.Close()

	info := &ZipDFUInfo{Path: path, InitPackets: make(map[string]InitPacketInfo)}
	for _, f := range r.File {
		if !strings.HasSuffix(f.Name, ".dat") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("opening %s in %q: %w", f.Name, path, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("reading %s in %q: %w", f.Name, path, err)
		}
		packet, err := parseDATInitPacket(data)
		if err != nil {
			return nil, fmt.Errorf("parsing %s in %q: %w", f.Name, path, err)
		}
		info.InitPackets[f.Name] = packet
	}
	return info, nil
}

// parseDATInitPacket walks the signed init packet protobuf and extracts
// fw_version (field 1) and type (field 4) from InitCommand. It does not
// verify the signature — the bootloader handles that during the real DFU
// transaction.
//
// Structure, per Nordic's dfu-cc.proto:
//
//	Packet                   (top level)
//	├── signed_command (2)   → SignedCommand
//	│   ├── command (1)      → Command
//	│   │   ├── op_code (1)
//	│   │   └── init (2)     → InitCommand
//	│   │       ├── fw_version (1)  ← what we want
//	│   │       ├── hw_version (2)
//	│   │       ├── sd_req (3)
//	│   │       ├── type (4)        ← what we want
//	│   │       └── ...
//	│   ├── signature_type (2)
//	│   └── signature (3)
//	└── (unsigned_command not used by nrfutil-generated zips)
func parseDATInitPacket(data []byte) (InitPacketInfo, error) {
	signedCmd, err := findFieldLenDelim(data, 2)
	if err != nil {
		return InitPacketInfo{}, fmt.Errorf("read Packet.signed_command: %w", err)
	}
	command, err := findFieldLenDelim(signedCmd, 1)
	if err != nil {
		return InitPacketInfo{}, fmt.Errorf("read SignedCommand.command: %w", err)
	}
	initCmd, err := findFieldLenDelim(command, 2)
	if err != nil {
		return InitPacketInfo{}, fmt.Errorf("read Command.init: %w", err)
	}
	fwVersion, err := findFieldVarint(initCmd, 1)
	if err != nil {
		return InitPacketInfo{}, fmt.Errorf("read InitCommand.fw_version: %w", err)
	}
	fwType, err := findFieldVarint(initCmd, 4)
	if err != nil {
		return InitPacketInfo{}, fmt.Errorf("read InitCommand.type: %w", err)
	}
	return InitPacketInfo{
		FwVersion: uint32(fwVersion),
		FwType:    FwType(fwType),
	}, nil
}

// findFieldVarint scans a protobuf message for the first varint field with the
// given field number and returns its value. Fields with other wire types are
// skipped.
func findFieldVarint(data []byte, target uint32) (uint64, error) {
	for offset := 0; offset < len(data); {
		tag, next, err := readVarint(data, offset)
		if err != nil {
			return 0, err
		}
		offset = next
		field := uint32(tag >> 3)
		wire := uint32(tag & 7)
		switch wire {
		case 0: // varint
			v, next, err := readVarint(data, offset)
			if err != nil {
				return 0, err
			}
			offset = next
			if field == target {
				return v, nil
			}
		case 2: // length-delimited
			length, next, err := readVarint(data, offset)
			if err != nil {
				return 0, err
			}
			offset = next + int(length)
			if offset > len(data) {
				return 0, fmt.Errorf("length-delimited field %d out of bounds", field)
			}
		case 1: // 64-bit fixed
			offset += 8
		case 5: // 32-bit fixed
			offset += 4
		default:
			return 0, fmt.Errorf("unsupported wire type %d for field %d", wire, field)
		}
	}
	return 0, fmt.Errorf("field %d not found", target)
}

// findFieldLenDelim scans a protobuf message for the first length-delimited
// field with the given field number and returns its value bytes.
func findFieldLenDelim(data []byte, target uint32) ([]byte, error) {
	for offset := 0; offset < len(data); {
		tag, next, err := readVarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		field := uint32(tag >> 3)
		wire := uint32(tag & 7)
		switch wire {
		case 0: // varint
			_, next, err := readVarint(data, offset)
			if err != nil {
				return nil, err
			}
			offset = next
		case 2: // length-delimited
			length, next, err := readVarint(data, offset)
			if err != nil {
				return nil, err
			}
			offset = next
			end := offset + int(length)
			if end > len(data) {
				return nil, fmt.Errorf("length-delimited field %d out of bounds", field)
			}
			if field == target {
				return data[offset:end], nil
			}
			offset = end
		case 1:
			offset += 8
		case 5:
			offset += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d for field %d", wire, field)
		}
	}
	return nil, fmt.Errorf("field %d not found", target)
}

// readVarint reads a protobuf varint from data starting at offset, returning
// the decoded value and the new offset. Supports up to 64-bit varints.
func readVarint(data []byte, offset int) (uint64, int, error) {
	var result uint64
	for shift := 0; ; shift += 7 {
		if offset >= len(data) {
			return 0, offset, fmt.Errorf("varint truncated")
		}
		b := data[offset]
		offset++
		result |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			return result, offset, nil
		}
		if shift >= 63 {
			return 0, offset, fmt.Errorf("varint too long")
		}
	}
}

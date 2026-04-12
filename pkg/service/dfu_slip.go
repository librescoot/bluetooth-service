package service

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/tarm/serial"
)

// Nordic Secure DFU serial protocol opcodes we use for version queries.
const (
	nrfDFUOpFirmwareVersion byte = 0x0B
	nrfDFUOpResponse        byte = 0x60

	nrfDFUResCodeSuccess byte = 0x01

	// Image types returned by GET_FIRMWARE_VERSION.
	nrfDFUFirmwareTypeSoftDevice  byte = 0x00
	nrfDFUFirmwareTypeApplication byte = 0x01
	nrfDFUFirmwareTypeBootloader  byte = 0x02
	nrfDFUFirmwareTypeUnknown     byte = 0xFF

	// Image indices passed as arguments to GET_FIRMWARE_VERSION.
	nrfDFUImageIdxBootloader  byte = 0
	nrfDFUImageIdxSoftDevice  byte = 1
	nrfDFUImageIdxApplication byte = 2

	// SLIP framing bytes.
	slipEND    byte = 0xC0
	slipESC    byte = 0xDB
	slipESCEnd byte = 0xDC
	slipESCEsc byte = 0xDD
)

// DFUFirmwareVersionInfo is the decoded response to NRF_DFU_OP_FIRMWARE_VERSION.
type DFUFirmwareVersionInfo struct {
	ImageType byte
	Version   uint32
	Addr      uint32
	Length    uint32
}

// QueryDFUFirmwareVersion sends a Nordic DFU serial GET_FIRMWARE_VERSION
// request for the given image index and returns the parsed response. The nRF
// must already be in DFU bootloader mode — typically call this right after
// enterDFUMode() succeeds and before runNordicDFU().
//
// The serial device must not be held open by any other process or goroutine
// when this function runs; it opens, queries, and closes the port on its own.
func QueryDFUFirmwareVersion(device string, baud int, imageIdx byte) (*DFUFirmwareVersionInfo, error) {
	port, err := serial.OpenPort(&serial.Config{
		Name:        device,
		Baud:        baud,
		Size:        8,
		Parity:      serial.ParityNone,
		StopBits:    serial.Stop1,
		ReadTimeout: 2 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", device, err)
	}
	defer port.Close()

	// Request payload: opcode + image index.
	req := slipEncode([]byte{nrfDFUOpFirmwareVersion, imageIdx})
	if _, err := port.Write(req); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}

	// Response payload is 16 bytes before SLIP framing.
	// Read with a generous cap in case the bootloader emits leading noise or a
	// double-framed empty packet.
	packet, err := slipReadPacket(port, 64)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return parseDFUFirmwareVersionResponse(packet)
}

// parseDFUFirmwareVersionResponse validates and decodes the 16-byte response
// to NRF_DFU_OP_FIRMWARE_VERSION.
//
//	Byte 0:   0x60 (NRF_DFU_OP_RESPONSE)
//	Byte 1:   0x0B (request opcode)
//	Byte 2:   result code (0x01 = SUCCESS)
//	Byte 3:   image type (0x00 SD, 0x01 APP, 0x02 BL, 0xFF UNKNOWN)
//	Bytes 4-7:   firmware version (little-endian uint32)
//	Bytes 8-11:  image start address (little-endian uint32)
//	Bytes 12-15: image length (little-endian uint32)
func parseDFUFirmwareVersionResponse(packet []byte) (*DFUFirmwareVersionInfo, error) {
	if len(packet) < 16 {
		return nil, fmt.Errorf("response too short: %d bytes (expected 16)", len(packet))
	}
	if packet[0] != nrfDFUOpResponse {
		return nil, fmt.Errorf("unexpected response marker 0x%02x (expected 0x%02x)",
			packet[0], nrfDFUOpResponse)
	}
	if packet[1] != nrfDFUOpFirmwareVersion {
		return nil, fmt.Errorf("response for wrong opcode 0x%02x (expected 0x%02x)",
			packet[1], nrfDFUOpFirmwareVersion)
	}
	if packet[2] != nrfDFUResCodeSuccess {
		return nil, fmt.Errorf("bootloader returned result code 0x%02x", packet[2])
	}
	return &DFUFirmwareVersionInfo{
		ImageType: packet[3],
		Version:   binary.LittleEndian.Uint32(packet[4:8]),
		Addr:      binary.LittleEndian.Uint32(packet[8:12]),
		Length:    binary.LittleEndian.Uint32(packet[12:16]),
	}, nil
}

// slipEncode wraps a payload in SLIP framing, escaping any occurrences of
// 0xC0 and 0xDB in the payload and appending a single 0xC0 end marker.
func slipEncode(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+2)
	for _, b := range payload {
		switch b {
		case slipEND:
			out = append(out, slipESC, slipESCEnd)
		case slipESC:
			out = append(out, slipESC, slipESCEsc)
		default:
			out = append(out, b)
		}
	}
	return append(out, slipEND)
}

// slipReadPacket reads SLIP-framed bytes from r until a complete packet is
// recovered (terminated by 0xC0) or maxSize bytes have been buffered. Leading
// 0xC0 end-markers (from prior packets or empty frames) are skipped.
func slipReadPacket(r io.Reader, maxSize int) ([]byte, error) {
	buf := make([]byte, 1)
	out := make([]byte, 0, 32)
	inEscape := false
	for len(out) < maxSize {
		n, err := r.Read(buf)
		if err != nil {
			if err == io.EOF && len(out) == 0 && !inEscape {
				return nil, io.EOF
			}
			if err == io.EOF {
				return nil, fmt.Errorf("SLIP packet truncated: %d bytes before EOF", len(out))
			}
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("timeout waiting for SLIP packet")
		}
		b := buf[0]
		if inEscape {
			switch b {
			case slipESCEnd:
				out = append(out, slipEND)
			case slipESCEsc:
				out = append(out, slipESC)
			default:
				return nil, fmt.Errorf("bad SLIP escape: 0xDB 0x%02x", b)
			}
			inEscape = false
			continue
		}
		switch b {
		case slipEND:
			if len(out) > 0 {
				return out, nil
			}
			// Leading end marker — consume and keep reading.
		case slipESC:
			inEscape = true
		default:
			out = append(out, b)
		}
	}
	return nil, fmt.Errorf("SLIP packet exceeded %d bytes", maxSize)
}

package service

import (
	"bytes"
	"io"
	"testing"
)

func TestSlipEncode(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{
			name: "empty payload gives bare end marker",
			in:   nil,
			want: []byte{0xC0},
		},
		{
			name: "GET_FIRMWARE_VERSION request",
			in:   []byte{0x0B, 0x00},
			want: []byte{0x0B, 0x00, 0xC0},
		},
		{
			name: "escape 0xC0 to DB DC",
			in:   []byte{0xC0},
			want: []byte{0xDB, 0xDC, 0xC0},
		},
		{
			name: "escape 0xDB to DB DD",
			in:   []byte{0xDB},
			want: []byte{0xDB, 0xDD, 0xC0},
		},
		{
			name: "mixed payload with both escape bytes",
			in:   []byte{0x01, 0xC0, 0x02, 0xDB, 0x03},
			want: []byte{0x01, 0xDB, 0xDC, 0x02, 0xDB, 0xDD, 0x03, 0xC0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := slipEncode(tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("got %x, want %x", got, tc.want)
			}
		})
	}
}

func TestSlipReadPacket(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want []byte
	}{
		{
			name: "plain 16-byte response ending in end marker",
			raw: []byte{
				0x60, 0x0B, 0x01, 0x02,
				0x04, 0x00, 0x00, 0x00,
				0x00, 0x80, 0x0F, 0x00,
				0xD0, 0x55, 0x00, 0x00,
				0xC0,
			},
			want: []byte{
				0x60, 0x0B, 0x01, 0x02,
				0x04, 0x00, 0x00, 0x00,
				0x00, 0x80, 0x0F, 0x00,
				0xD0, 0x55, 0x00, 0x00,
			},
		},
		{
			name: "leading end marker is consumed before the real packet starts",
			raw:  []byte{0xC0, 0x60, 0x0B, 0x01, 0x02, 0xC0},
			want: []byte{0x60, 0x0B, 0x01, 0x02},
		},
		{
			name: "payload with escaped 0xC0 gets un-escaped",
			raw: []byte{
				0x60, 0x0B, 0x01, 0x02,
				0xDB, 0xDC, 0xDB, 0xDC, 0xDB, 0xDC, 0xDB, 0xDC,
				0x00, 0x80, 0x0F, 0x00,
				0xD0, 0x55, 0x00, 0x00,
				0xC0,
			},
			want: []byte{
				0x60, 0x0B, 0x01, 0x02,
				0xC0, 0xC0, 0xC0, 0xC0,
				0x00, 0x80, 0x0F, 0x00,
				0xD0, 0x55, 0x00, 0x00,
			},
		},
		{
			name: "payload with escaped 0xDB gets un-escaped",
			raw:  []byte{0x01, 0xDB, 0xDD, 0x02, 0xC0},
			want: []byte{0x01, 0xDB, 0x02},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := slipReadPacket(bytes.NewReader(tc.raw), 64)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("got %x, want %x", got, tc.want)
			}
		})
	}
}

func TestSlipReadPacketErrors(t *testing.T) {
	t.Run("bad escape sequence", func(t *testing.T) {
		_, err := slipReadPacket(bytes.NewReader([]byte{0x01, 0xDB, 0xFF, 0xC0}), 64)
		if err == nil {
			t.Error("expected error on bad escape sequence")
		}
	})
	t.Run("eof mid-packet without end marker", func(t *testing.T) {
		_, err := slipReadPacket(bytes.NewReader([]byte{0x01, 0x02}), 64)
		if err == nil {
			t.Error("expected error on truncated packet")
		}
	})
	t.Run("empty reader returns EOF", func(t *testing.T) {
		_, err := slipReadPacket(bytes.NewReader(nil), 64)
		if err != io.EOF {
			t.Errorf("expected io.EOF, got %v", err)
		}
	})
	t.Run("packet exceeds max size", func(t *testing.T) {
		// 10 bytes of payload without a terminator, max=5.
		_, err := slipReadPacket(bytes.NewReader([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 0xC0}), 5)
		if err == nil {
			t.Error("expected error on oversized packet")
		}
	})
}

func TestParseDFUFirmwareVersionResponse(t *testing.T) {
	t.Run("valid bootloader version 4 response", func(t *testing.T) {
		packet := []byte{
			nrfDFUOpResponse,             // 0x60
			nrfDFUOpFirmwareVersion,      // 0x0B
			nrfDFUResCodeSuccess,         // 0x01
			nrfDFUFirmwareTypeBootloader, // 0x02
			0x04, 0x00, 0x00, 0x00,       // version = 4
			0x00, 0x80, 0x0F, 0x00, // addr = 0xF8000
			0xD0, 0x55, 0x00, 0x00, // length = 0x55D0
		}
		info, err := parseDFUFirmwareVersionResponse(packet)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.ImageType != nrfDFUFirmwareTypeBootloader {
			t.Errorf("image_type: got 0x%02x, want 0x02", info.ImageType)
		}
		if info.Version != 4 {
			t.Errorf("version: got %d, want 4", info.Version)
		}
		if info.Addr != 0x000F8000 {
			t.Errorf("addr: got 0x%08x, want 0x000F8000", info.Addr)
		}
		if info.Length != 0x000055D0 {
			t.Errorf("length: got 0x%08x, want 0x000055D0", info.Length)
		}
	})

	t.Run("too short", func(t *testing.T) {
		_, err := parseDFUFirmwareVersionResponse([]byte{0x60, 0x0B, 0x01, 0x02})
		if err == nil {
			t.Error("expected error on short response")
		}
	})

	t.Run("wrong opcode marker", func(t *testing.T) {
		packet := make([]byte, 16)
		packet[0] = 0x61 // not 0x60
		packet[1] = nrfDFUOpFirmwareVersion
		packet[2] = nrfDFUResCodeSuccess
		packet[3] = nrfDFUFirmwareTypeBootloader
		_, err := parseDFUFirmwareVersionResponse(packet)
		if err == nil {
			t.Error("expected error on wrong opcode marker")
		}
	})

	t.Run("wrong request opcode", func(t *testing.T) {
		packet := make([]byte, 16)
		packet[0] = nrfDFUOpResponse
		packet[1] = 0x09 // PING, not FIRMWARE_VERSION
		packet[2] = nrfDFUResCodeSuccess
		packet[3] = nrfDFUFirmwareTypeBootloader
		_, err := parseDFUFirmwareVersionResponse(packet)
		if err == nil {
			t.Error("expected error on wrong request opcode")
		}
	})

	t.Run("non-success result code", func(t *testing.T) {
		packet := make([]byte, 16)
		packet[0] = nrfDFUOpResponse
		packet[1] = nrfDFUOpFirmwareVersion
		packet[2] = 0x04 // invalid parameter or similar
		packet[3] = nrfDFUFirmwareTypeBootloader
		_, err := parseDFUFirmwareVersionResponse(packet)
		if err == nil {
			t.Error("expected error on non-success result code")
		}
	})

	t.Run("end-to-end: encode request, decode real response", func(t *testing.T) {
		// Round-trip: SLIP-encode the request, verify it decodes back to the
		// expected wire bytes.
		req := slipEncode([]byte{nrfDFUOpFirmwareVersion, nrfDFUImageIdxBootloader})
		if !bytes.Equal(req, []byte{0x0B, 0x00, 0xC0}) {
			t.Errorf("request: got %x, want 0b00c0", req)
		}

		// A real bootloader response encoded as SLIP. No bytes in the payload
		// need escaping so the wire form is just payload + 0xC0.
		wire := []byte{
			nrfDFUOpResponse, nrfDFUOpFirmwareVersion, nrfDFUResCodeSuccess, nrfDFUFirmwareTypeBootloader,
			0x04, 0x00, 0x00, 0x00,
			0x00, 0x80, 0x0F, 0x00,
			0xD0, 0x55, 0x00, 0x00,
			slipEND,
		}
		packet, err := slipReadPacket(bytes.NewReader(wire), 64)
		if err != nil {
			t.Fatalf("slipReadPacket: %v", err)
		}
		info, err := parseDFUFirmwareVersionResponse(packet)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if info.Version != 4 {
			t.Errorf("version: got %d, want 4", info.Version)
		}
		if info.ImageType != nrfDFUFirmwareTypeBootloader {
			t.Errorf("image_type: got 0x%02x, want BOOTLOADER (0x02)", info.ImageType)
		}
	})
}

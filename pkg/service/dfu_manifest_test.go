package service

import (
	"testing"
)

// These tests use hex-encoded init packets lifted directly from real Nordic
// DFU zips produced by utils/firmware-release.sh on nrf-fw v2.1.0-ls.
// They exercise the hand-rolled protobuf walker against real-world data,
// not a synthetic fixture.

func TestParseDATInitPacket_Application(t *testing.T) {
	// nrf-fw.dat from nrf-fw-app-v2.1.0-ls.zip (209 bytes).
	// APP_VERSION=2, sd_req=[0x100,0xB6] (the wider-compat app-only variant).
	dat := mustDecodeHex(t,
		"12ce010a8701080112820108021034"+
			"1a048002b601200028003000"+
			"38d48407"+
			"422408031220288aca0adc92a43c105b5a7e617aff187c41fe047d82880f982628cc3af7d5c848"+
			"00"+
			"5244080312402002aa8472d2011bb1f8e3e3a52c86307ee2f220fa1e1bffec5b416a0dc6e81b54"+
			"7fb59a7a3652d81a0af5e6e2be365710e429c2b9694072df2a6a901d191a9b10001a40571083"+
			"6700d99968ac5c93a96bb2ee344e2f0fc487ba4bb72115825f706ade2b7b05cfe277284d68dc"+
			"588ed738634486a152051fa6cf2198674ecdabfcf0610a",
	)
	info, err := parseDATInitPacket(dat)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.FwVersion != 2 {
		t.Errorf("fw_version: got %d, want 2", info.FwVersion)
	}
	if info.FwType != FwTypeApplication {
		t.Errorf("fw_type: got %s, want APPLICATION", info.FwType)
	}
}

func TestParseDATInitPacket_FullZipApp(t *testing.T) {
	// nrf-fw.dat from nrf-fw-full-v2.1.0-ls.zip (207 bytes).
	// APP_VERSION=2, but narrower sd_req=[0x100] because it ships alongside the
	// softdevice update.
	dat := mustDecodeHex(t,
		"12cc010a8501080112800108021034"+
			"1a028002200028003000"+
			"38d48407"+
			"422408031220288aca0adc92a43c105b5a7e617aff187c41fe047d82880f982628cc3af7d5c848"+
			"00"+
			"524408031240"+
			"72b44ddf54a6b4953af16f835f29d878954e68144b690939c6ebe7c77c1c1790b056665084bd"+
			"f11cca13fd897838e65ef5e3102731a50a46c09450aee0f00d"+
			"7a10001a4037"+
			"99c778523d56078b5d6ccdb29db001d6c4abf3147f2391c6b400c7d7df5ba22bda4322cc54"+
			"9019bd6e1bc3bceda246d0b2dbb4de7135da2d2d6eea4886cc00",
	)
	info, err := parseDATInitPacket(dat)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.FwVersion != 2 {
		t.Errorf("fw_version: got %d, want 2", info.FwVersion)
	}
	if info.FwType != FwTypeApplication {
		t.Errorf("fw_type: got %s, want APPLICATION", info.FwType)
	}
}

func TestParseDATInitPacket_BootloaderSoftdevice(t *testing.T) {
	// sd_bl.dat from nrf-fw-full-v2.1.0-ls.zip. Representative bytes:
	// BL_VERSION=4, fw_type=SOFTDEVICE_BOOTLOADER (3), hw_version=52, sd_req=[0x100].
	// Exact hex isn't reproducible without re-running nrfutil (signature is
	// random), but the structural assertions are still worth checking.
	//
	// Synthetic-but-schema-correct init packet:
	//   Packet { signed_command = SignedCommand { command = Command { op_code=INIT, init = InitCommand { fw_version=4, hw_version=52, sd_req=[256], type=SOFTDEVICE_BOOTLOADER, sd_size=153140, bl_size=22220 } }, signature_type=0, signature=<empty> } }
	dat := buildSyntheticSDBLDat(4)
	info, err := parseDATInitPacket(dat)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if info.FwVersion != 4 {
		t.Errorf("fw_version: got %d, want 4", info.FwVersion)
	}
	if info.FwType != FwTypeSoftDeviceBootloader {
		t.Errorf("fw_type: got %s, want SOFTDEVICE_BOOTLOADER", info.FwType)
	}
}

func TestReadVarint(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want uint64
	}{
		{"zero", []byte{0x00}, 0},
		{"small", []byte{0x01}, 1},
		{"one-byte-max", []byte{0x7F}, 127},
		{"two-byte", []byte{0x80, 0x01}, 128},
		{"0x100 encoded as 80 02", []byte{0x80, 0x02}, 256},
		{"0xB6 encoded as b6 01", []byte{0xB6, 0x01}, 182}, // 0xB6
		{"app_size 115284", []byte{0xD4, 0x84, 0x07}, 115284},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, _, err := readVarint(tc.in, 0)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if v != tc.want {
				t.Errorf("got %d, want %d", v, tc.want)
			}
		})
	}
}

func TestReadVarint_Truncated(t *testing.T) {
	if _, _, err := readVarint([]byte{0x80}, 0); err == nil {
		t.Error("expected error on truncated varint")
	}
	if _, _, err := readVarint([]byte{}, 0); err == nil {
		t.Error("expected error on empty input")
	}
}

// mustDecodeHex is a test helper that decodes a hex string (with optional
// whitespace) or fails the test.
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	// Strip any whitespace.
	cleaned := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		cleaned = append(cleaned, c)
	}
	if len(cleaned)%2 != 0 {
		t.Fatalf("odd-length hex: %q", s)
	}
	out := make([]byte, len(cleaned)/2)
	for i := 0; i < len(cleaned); i += 2 {
		hi := hexNibble(cleaned[i])
		lo := hexNibble(cleaned[i+1])
		if hi == 0xff || lo == 0xff {
			t.Fatalf("bad hex at %d: %q", i, s)
		}
		out[i/2] = (hi << 4) | lo
	}
	return out
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0xff
}

// buildSyntheticSDBLDat builds a protobuf byte sequence that matches Nordic's
// Packet schema for an SD+BL init packet, with the specified fw_version.
// We don't care about the signature — the parser doesn't verify it.
func buildSyntheticSDBLDat(blVersion uint32) []byte {
	// InitCommand:
	//   fw_version (field 1, varint) = blVersion
	//   hw_version (field 2, varint) = 52
	//   sd_req     (field 3, bytes)  = [0x80 0x02]  (packed uint32 = 256)
	//   type       (field 4, varint) = 3 (SOFTDEVICE_BOOTLOADER)
	initCmd := []byte{}
	initCmd = appendTag(initCmd, 1, 0) // varint field 1
	initCmd = appendVarint(initCmd, uint64(blVersion))
	initCmd = appendTag(initCmd, 2, 0)
	initCmd = appendVarint(initCmd, 52)
	initCmd = appendTag(initCmd, 3, 2) // len-delim for packed repeated
	initCmd = appendVarint(initCmd, 2)
	initCmd = append(initCmd, 0x80, 0x02)
	initCmd = appendTag(initCmd, 4, 0)
	initCmd = appendVarint(initCmd, 3)

	// Command:
	//   op_code (field 1, varint) = 1 (INIT)
	//   init    (field 2, bytes)  = initCmd
	command := []byte{}
	command = appendTag(command, 1, 0)
	command = appendVarint(command, 1)
	command = appendTag(command, 2, 2)
	command = appendVarint(command, uint64(len(initCmd)))
	command = append(command, initCmd...)

	// SignedCommand:
	//   command (field 1, bytes) = command
	//   signature_type (field 2, varint) = 0
	//   signature (field 3, bytes) = <empty>
	signed := []byte{}
	signed = appendTag(signed, 1, 2)
	signed = appendVarint(signed, uint64(len(command)))
	signed = append(signed, command...)
	signed = appendTag(signed, 2, 0)
	signed = appendVarint(signed, 0)
	signed = appendTag(signed, 3, 2)
	signed = appendVarint(signed, 0)

	// Packet:
	//   signed_command (field 2, bytes) = signed
	packet := []byte{}
	packet = appendTag(packet, 2, 2)
	packet = appendVarint(packet, uint64(len(signed)))
	packet = append(packet, signed...)

	return packet
}

func appendTag(dst []byte, field uint32, wire uint32) []byte {
	return appendVarint(dst, uint64(field<<3|wire))
}

func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

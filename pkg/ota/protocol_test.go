package ota

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Golden vectors shared with the Flutter implementation
// (unustasis test/ota_protocol_test.dart) — keep in sync.
func TestGoldenVectors(t *testing.T) {
	var sha [32]byte
	for i := range sha {
		sha[i] = byte(i)
	}

	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{
			"start",
			EncodeStart(Start{
				Version:   1,
				Component: ComponentMDB,
				ChunkSize: 240,
				TotalSize: 0x01020304,
				SHA256:    sha,
				BundleID:  "bundle-v1",
			}),
			"010100f000040302010001020304050607" +
				"08090a0b0c0d0e0f1011121314151617" +
				"18191a1b1c1d1e1f" +
				"09" + hex.EncodeToString([]byte("bundle-v1")),
		},
		{
			"start_staged",
			EncodeStart(Start{
				Version:   1,
				Component: ComponentMDB,
				ChunkSize: 240,
				TotalSize: 0x01020304,
				SHA256:    sha,
				BundleID:  "bundle-v1",
				Staged:    true,
			}),
			"020100f000040302010001020304050607" +
				"08090a0b0c0d0e0f1011121314151617" +
				"18191a1b1c1d1e1f" +
				"09" + hex.EncodeToString([]byte("bundle-v1")),
		},
		{"apply_staged", EncodeApplyStaged(ComponentDBC), "0601"},
		{"data", EncodeData(0xAABBCCDD, []byte{0xde, 0xad}), "ddccbbaadead"},
		{"start_ack", EncodeStartAck(StartResume, 0x00010000, 64, 16, 240), "8100000001004000" + "10" + "f000"},
		{"ack", EncodeAck(AckFlagRewind, 0x12345678), "820178563412"},
		{"complete_ack", EncodeCompleteAck(CompleteOK), "8300"},
		{"install_progress", EncodeInstallProgress(PhaseInstalling, 42, "ok"), "84012a026f6b"},
		{"install_up_to_date", EncodeInstallProgress(PhaseUpToDate, 100, ""), "84076400"},
		{"error", EncodeError(ErrOverflow, "E:overflow"), "86020a" + hex.EncodeToString([]byte("E:overflow"))},
		{"abort_ack", EncodeAbortAck(), "85"},
	}
	for _, tc := range tests {
		want, err := hex.DecodeString(tc.want)
		if err != nil {
			t.Fatalf("%s: bad vector: %v", tc.name, err)
		}
		if !bytes.Equal(tc.got, want) {
			t.Errorf("%s:\n got %x\nwant %x", tc.name, tc.got, want)
		}
	}
}

func TestStartRoundTrip(t *testing.T) {
	var sha [32]byte
	copy(sha[:], bytes.Repeat([]byte{0xAB}, 32))
	in := Start{
		Version:   1,
		Component: ComponentDBC,
		ChunkSize: 128,
		TotalSize: 31457280,
		SHA256:    sha,
		BundleID:  "librescoot-unu-dbc-nightly-20260701",
	}
	out, err := DecodeStart(EncodeStart(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestDecodeStartErrors(t *testing.T) {
	if _, err := DecodeStart([]byte{OpStart, 1, 0}); err == nil {
		t.Error("short START accepted")
	}
	var sha [32]byte
	p := EncodeStart(Start{Version: 1, ChunkSize: 240, TotalSize: 10, SHA256: sha, BundleID: "x"})
	p[0] = OpComplete
	if _, err := DecodeStart(p); err == nil {
		t.Error("wrong opcode accepted")
	}
	p[0] = OpStart
	p = p[:len(p)-1] // truncate bundle id
	if _, err := DecodeStart(p); err == nil {
		t.Error("truncated bundle id accepted")
	}

	// START_STAGED carries the same payload and must decode to the same fields
	// with the staging flag set.
	staged := EncodeStart(Start{Version: 1, ChunkSize: 240, TotalSize: 10, SHA256: sha, BundleID: "x", Staged: true})
	parsed, err := DecodeStart(staged)
	if err != nil || !parsed.Staged || parsed.BundleID != "x" {
		t.Fatalf("staged START decode = %+v, %v", parsed, err)
	}
}

func TestDecodeApplyStaged(t *testing.T) {
	component, err := DecodeApplyStaged(EncodeApplyStaged(ComponentDBC))
	if err != nil || component != ComponentDBC {
		t.Fatalf("component = %d, %v", component, err)
	}
	for _, bad := range [][]byte{
		{OpApplyStaged},
		{OpApplyStaged, ComponentMDB, 0},
		{OpStart, ComponentMDB},
	} {
		if _, err := DecodeApplyStaged(bad); err == nil {
			t.Errorf("DecodeApplyStaged(%x) accepted", bad)
		}
	}
}

func TestBundleIDValidation(t *testing.T) {
	valid := []string{
		"x",
		"bundle-v1",
		"librescoot-unu-mdb-nightly-20260701T000000",
		"librescoot-unu-mdb-nightly-20260701T000000.delta",
		"librescoot-unu-mdb-nightly-20260701T000000.hdelta",
		string(bytes.Repeat([]byte{'a'}, 57)) + ".hdelta",
		"librescoot-unu-dbc-v1.2.3.mender",
		"A_b-c.d",
	}
	invalid := []string{
		"",
		".hidden",
		"..",
		"../escape",
		"a/b",
		"/abs",
		"-leading-dash",
		"_leading_underscore",
		"space here",
		"null\x00byte",
		string(bytes.Repeat([]byte{'a'}, 65)),
	}
	for _, id := range valid {
		if !validBundleID(id) {
			t.Errorf("validBundleID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if validBundleID(id) {
			t.Errorf("validBundleID(%q) = true, want false", id)
		}
	}

	// DecodeStart enforces it end to end
	var sha [32]byte
	p := EncodeStart(Start{Version: 1, ChunkSize: 240, TotalSize: 10, SHA256: sha, BundleID: "a/../../etc"})
	if _, err := DecodeStart(p); err == nil {
		t.Error("traversal bundle id accepted")
	}
}

func TestDecodeData(t *testing.T) {
	off, data, err := DecodeData(EncodeData(1024, []byte{1, 2, 3}))
	if err != nil || off != 1024 || !bytes.Equal(data, []byte{1, 2, 3}) {
		t.Errorf("data round trip failed: off=%d data=%x err=%v", off, data, err)
	}
	if _, _, err := DecodeData([]byte{1, 2, 3, 4}); err == nil {
		t.Error("short DATA accepted")
	}
}

func TestTruncatedMessages(t *testing.T) {
	long := string(bytes.Repeat([]byte{'a'}, 100))
	p := EncodeInstallProgress(PhaseFailure, 0, long)
	if len(p) != 4+60 {
		t.Errorf("progress message not truncated: %d", len(p))
	}
	if int(p[3]) != 60 {
		t.Errorf("msg_len mismatch: %d", p[3])
	}
	e := EncodeError(ErrInternal, long)
	if len(e) != 3+60 || int(e[2]) != 60 {
		t.Errorf("error message not truncated: len=%d msg_len=%d", len(e), e[2])
	}
}

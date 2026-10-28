package filetransfer

import (
	"encoding/hex"
	"testing"
)

func TestWireVectors(t *testing.T) {
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i)
	}
	r := Request{Op: OpPut, ID: 0x11223344, Budget: 244, Store: StoreInbox, Name: "sample.bin", Chunk: 128, Size: 3000, Hash: hash}
	const put = "030144332211f400010a73616d706c652e62696e8000b80b000000000000000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got := hex.EncodeToString(EncodeRequest(r)); got != put {
		t.Fatalf("PUT: %s", got)
	}
	const data = "ddccbbaa0500000001000000000102feff"
	if got := hex.EncodeToString(EncodeData(0xaabbccdd, 0x100000005, []byte{0, 1, 2, 254, 255})); got != data {
		t.Fatalf("DATA: %s", got)
	}
	s, w, _ := fixture(t)
	s.startAck(&transfer{id: 0xaabbccdd, offset: 256, request: r, stored: &storedFile{info: FileInfo{Size: 3000, Hash: hash}}})
	const start = "8300ddccbbaa0001000000000000b80b00000000000080000800000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if got := hex.EncodeToString(message(t, w, r.ID)); got != start {
		t.Fatalf("START: %s", got)
	}
}

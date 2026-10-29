package usock

import (
	"bytes"
	"encoding/hex"
	"log"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

type memoryPort struct{ bytes.Buffer }

func (p *memoryPort) Close() error { return nil }

func TestExtendedTransportRedactsPayloadAndFrameDumps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subtype uint16
		value   interface{}
	}{
		{"hash request", 0x0401, "hash:read:1234abcd:20:battery:0/serial-number"},
		{"hash response", 0x0402, "r:p:1234abcd:0:WyJzZWNyZXQtaWRlbnRpdHkiXQ"},
		{"byte request", 0x0401, []byte("hash:read:1234abcd:20:battery:0/private-field")},
		{"malformed payload", 0, "hash:read:1234abcd:20:battery:0/private-field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := cbor.Marshal(map[uint16]map[uint16]interface{}{0x0400: {tc.subtype: tc.value}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.subtype == 0 {
				data = []byte(tc.value.(string))
			}
			var output bytes.Buffer
			port := &memoryPort{}
			u := &USOCK{port: port, log: logger.NewLogger(log.New(&output, "", 0), logger.LogLevelDebug)}
			if err := u.WriteWithFrameID(0x00, data); err != nil {
				t.Fatal(err)
			}
			wire := append([]byte(nil), port.Bytes()...)
			var received *Payload
			u.SetSyncFrameIDs(0x00)
			u.handler = func(payload *Payload) { received = payload }
			for _, b := range wire {
				u.processByte(b)
			}
			if received == nil || !bytes.Equal(received.Data, data) {
				t.Fatal("redaction changed transmitted/received payload")
			}
			logs := output.String()
			for _, forbidden := range []string{
				"TX Payload:", "RX Payload:", "TX Complete Frame:",
				hex.EncodeToString(data), hex.EncodeToString(wire),
				"1234abcd", "serial-number", "private-field", "WyJzZWNyZXQtaWRlbnRpdHkiXQ",
			} {
				if strings.Contains(logs, forbidden) {
					t.Fatalf("sensitive transport log: %s", logs)
				}
			}
			if !strings.Contains(logs, "TX Frame: ID=0x00") || !strings.Contains(logs, "RX Frame: ID=0x00") {
				t.Fatal("safe transport metadata missing")
			}
		})
	}
}

func TestOtherTransportFrameLoggingUnchanged(t *testing.T) {
	var output bytes.Buffer
	port := &memoryPort{}
	u := &USOCK{port: port, log: logger.NewLogger(log.New(&output, "", 0), logger.LogLevelDebug)}
	data := []byte{1, 2, 3}
	if err := u.WriteWithFrameID(0x20, data); err != nil {
		t.Fatal(err)
	}
	for _, b := range port.Bytes() {
		u.processByte(b)
	}
	for _, expected := range []string{"TX Payload: 010203", "RX Payload: 010203", "TX Complete Frame:"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("missing %q", expected)
		}
	}
}

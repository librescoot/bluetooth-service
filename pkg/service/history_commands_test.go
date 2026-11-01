package service

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHistoryChunkAtSmallMTU(t *testing.T) {
	data := []byte(`{"trips":[{"id":"abc","distance_m":1234}]}`)
	snapshot := &historySnapshot{token: "12345678", data: data, until: time.Now().Add(time.Minute)}
	var got []byte
	for len(got) < len(data) {
		offset := strconv.FormatInt(int64(len(got)), 36)
		response := historyChunk(snapshot, snapshot.token, offset, "20", time.Now())
		prefix := "h:p:" + snapshot.token + ":" + offset + ":"
		if len(response) > 20 || !strings.HasPrefix(response, prefix) {
			t.Fatalf("chunk: %q", response)
		}
		chunk, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(response, prefix))
		if err != nil || len(chunk) == 0 {
			t.Fatalf("decode: %q %v", response, err)
		}
		got = append(got, chunk...)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q", got)
	}
	if got := historyChunk(snapshot, "bad", "0", "20", time.Now()); got != "h:x:session" {
		t.Fatal(got)
	}
	if got := historyChunk(snapshot, snapshot.token, "0", "19", time.Now()); got != "h:x:budget" {
		t.Fatal(got)
	}
}

func TestHistoryRPCSnapshot(t *testing.T) {
	s, mr := newVersionPushService(t)
	s.destIPC = s.ipc
	mr.Set("trip:history:ready", "1")
	requests := startCallServer(t, mr, "trip:history:rpc", func(env destinationEnvelope) []byte {
		return []byte(`{"ok":true,"payload":{"trips":[{"id":"abc"}]}}`)
	})
	request := base64.RawURLEncoding.EncodeToString([]byte(`{"limit":1}`))
	s.handleHistoryCommand("start:l:" + request)
	response := routeResponse(t, s.usock.(*mockUSOCK))
	fields := strings.Split(response, ":")
	if len(fields) != 4 || fields[0] != "h" || fields[1] != "s" {
		t.Fatalf("start: %q", response)
	}
	if env := <-requests; env.Method != "history.list" {
		t.Fatalf("method: %s", env.Method)
	}
	s.handleHistoryCommand("get:" + fields[2] + ":0:20")
	if got := routeResponse(t, s.usock.(*mockUSOCK)); !strings.HasPrefix(got, "h:p:") || len(got) > 20 {
		t.Fatal(got)
	}
	s.clearHistorySnapshot()
	s.handleHistoryCommand("get:" + fields[2] + ":0:20")
	if got := routeResponse(t, s.usock.(*mockUSOCK)); got != "h:x:session" {
		t.Fatal(got)
	}
}

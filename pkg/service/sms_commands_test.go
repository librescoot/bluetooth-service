package service

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSMSChunksSmallMTU(t *testing.T) {
	message := json.RawMessage(`{"id":"123","text":"Hällo 😀"}`)
	snapshot := &smsSnapshot{token: "1234", messages: []json.RawMessage{message}, until: time.Now().Add(time.Minute)}
	var assembled []byte
	for len(assembled) < len(message) {
		offset := strconv.FormatInt(int64(len(assembled)), 36)
		response := smsChunk(snapshot, "1234", "0", offset, "20", time.Now())
		if len(response) > 20 {
			t.Fatalf("oversized response %q", response)
		}
		prefix := "s:p:1234:" + offset + ":"
		if !strings.HasPrefix(response, prefix) {
			t.Fatalf("unexpected chunk %q", response)
		}
		data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(response, prefix))
		if err != nil || len(data) == 0 {
			t.Fatalf("chunk %q: %v", response, err)
		}
		assembled = append(assembled, data...)
	}
	if string(assembled) != string(message) {
		t.Fatalf("got %q", assembled)
	}
	offset := strconv.FormatInt(int64(len(assembled)), 36)
	if got := smsChunk(snapshot, "1234", "0", offset, "20", time.Now()); got != "s:e:1234:"+offset {
		t.Fatalf("end %q", got)
	}
	if got := smsChunk(snapshot, "bad", "0", "0", "20", time.Now()); got != "s:x:session" {
		t.Fatalf("token %q", got)
	}
	if got := smsChunk(snapshot, "1234", "0", "0", "19", time.Now()); got != "s:x:budget" {
		t.Fatalf("budget %q", got)
	}
}

func TestSMSSnapshotLifecycle(t *testing.T) {
	s, redis := newVersionPushService(t)
	if s.smsInboxAvailable() {
		t.Fatal("advertised SMS without a backend snapshot")
	}
	redis.HSet("sms:inbox", "snapshot", `{"version":1,"unread":1,"messages":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","text":"hello"}]}`)
	if !s.smsInboxAvailable() {
		t.Fatal("did not advertise SMS")
	}
	s.handleSMSCommand("start")
	response := routeResponse(t, s.usock.(*mockUSOCK))
	parts := strings.Split(response, ":")
	if len(parts) != 5 || parts[0] != "s" || parts[1] != "s" || parts[3] != "1" || parts[4] != "1" {
		t.Fatalf("start = %q", response)
	}
	token := parts[2]
	s.handleSMSCommand("get:" + token + ":0:0:20")
	if response := routeResponse(t, s.usock.(*mockUSOCK)); !strings.HasPrefix(response, "s:p:"+token+":0:") || len(response) > 20 {
		t.Fatalf("chunk = %q", response)
	}
	s.clearSMSSnapshot()
	s.handleSMSCommand("get:" + token + ":0:0:20")
	if response := routeResponse(t, s.usock.(*mockUSOCK)); response != "s:x:session" {
		t.Fatalf("after disconnect = %q", response)
	}
}

func TestSMSProjectionValidation(t *testing.T) {
	if _, err := parseSMSProjection(`{"version":1,"unread":1,"messages":[{"text":"hi"}]}`); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `{"version":2,"unread":0,"messages":[]}`, `{"version":1,"unread":2,"messages":[{}]}`} {
		if _, err := parseSMSProjection(raw); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

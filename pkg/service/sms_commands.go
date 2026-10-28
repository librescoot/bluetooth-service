package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const smsSnapshotMaxBytes = 1024 * 1024

type smsProjection struct {
	Version  int               `json:"version"`
	Unread   int               `json:"unread"`
	Messages []json.RawMessage `json:"messages"`
}

type smsSnapshot struct {
	token    string
	messages []json.RawMessage
	until    time.Time
}

func parseSMSProjection(raw string) (*smsProjection, error) {
	if len(raw) > smsSnapshotMaxBytes {
		return nil, fmt.Errorf("inbox too large")
	}
	var projection smsProjection
	if err := json.Unmarshal([]byte(raw), &projection); err != nil {
		return nil, err
	}
	if projection.Version != 1 || projection.Unread < 0 || len(projection.Messages) > 100 || projection.Unread > len(projection.Messages) {
		return nil, fmt.Errorf("invalid inbox")
	}
	for _, message := range projection.Messages {
		if len(message) > 32768 || !json.Valid(message) {
			return nil, fmt.Errorf("invalid message")
		}
	}
	return &projection, nil
}

func (s *Service) smsInboxAvailable() bool {
	if s.ipc == nil {
		return false
	}
	raw, err := s.ipc.HGet("sms:inbox", "snapshot")
	if err != nil {
		return false
	}
	_, err = parseSMSProjection(raw)
	return err == nil
}

func (s *Service) clearSMSSnapshot() {
	s.smsMu.Lock()
	s.smsSnapshot = nil
	s.smsGeneration++
	s.smsMu.Unlock()
}

func (s *Service) handleSMSCommand(cmd string) {
	if cmd == "start" {
		s.startSMSSnapshot()
		return
	}
	parts := strings.Split(cmd, ":")
	if len(parts) == 5 && parts[0] == "get" {
		s.smsMu.Lock()
		snapshot := s.smsSnapshot
		response := smsChunk(snapshot, parts[1], parts[2], parts[3], parts[4], time.Now())
		s.smsMu.Unlock()
		s.sendExtendedResponse(response)
		return
	}
	if len(parts) == 2 && (parts[0] == "read" || parts[0] == "delete") {
		if len(parts[1]) != 32 {
			s.sendExtendedResponse("s:x:id")
			return
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			s.sendExtendedResponse("s:x:id")
			return
		}
		payload, _ := json.Marshal(map[string]string{"action": parts[0], "id": parts[1]})
		if err := ipc.SendRequest(s.ipc, "sms:inbox:commands", string(payload)); err != nil {
			s.sendExtendedResponse("s:x:unavailable")
			return
		}
		s.sendExtendedResponse("s:queued")
		return
	}
	s.sendExtendedResponse("s:x:command")
}

func (s *Service) startSMSSnapshot() {
	if s.ipc == nil {
		s.sendExtendedResponse("s:x:unavailable")
		return
	}
	s.smsMu.Lock()
	generation := s.smsGeneration
	s.smsMu.Unlock()
	raw, err := s.ipc.HGet("sms:inbox", "snapshot")
	if err != nil {
		s.sendExtendedResponse("s:x:unavailable")
		return
	}
	projection, err := parseSMSProjection(raw)
	if err != nil {
		s.sendExtendedResponse("s:x:unavailable")
		return
	}
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.sendExtendedResponse("s:x:internal")
		return
	}
	token := hex.EncodeToString(random[:])
	s.smsMu.Lock()
	if generation != s.smsGeneration {
		s.smsMu.Unlock()
		return
	}
	s.smsSnapshot = &smsSnapshot{token: token, messages: projection.Messages, until: time.Now().Add(5 * time.Minute)}
	s.smsMu.Unlock()
	s.sendExtendedResponse(fmt.Sprintf("s:s:%s:%d:%d", token, len(projection.Messages), projection.Unread))
}

func smsChunk(snapshot *smsSnapshot, token, indexText, offsetText, budgetText string, now time.Time) string {
	if snapshot == nil || snapshot.token != token || now.After(snapshot.until) {
		return "s:x:session"
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index < 0 || index >= len(snapshot.messages) {
		return "s:x:index"
	}
	offset, err := strconv.ParseUint(offsetText, 36, 32)
	if err != nil || int(offset) > len(snapshot.messages[index]) {
		return "s:x:offset"
	}
	budget, err := strconv.Atoi(budgetText)
	if err != nil || budget < 20 || budget > 244 {
		return "s:x:budget"
	}
	snapshot.until = now.Add(5 * time.Minute)
	if int(offset) == len(snapshot.messages[index]) {
		return fmt.Sprintf("s:e:%s:%s", token, offsetText)
	}
	prefix := fmt.Sprintf("s:p:%s:%s:", token, offsetText)
	remaining := snapshot.messages[index][offset:]
	n := len(remaining)
	if n > budget {
		n = budget
	}
	for n > 0 && len(prefix)+base64.RawURLEncoding.EncodedLen(n) > budget {
		n--
	}
	if n == 0 {
		return "s:x:budget"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(remaining[:n])
}

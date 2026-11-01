package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const historyResponseLimit = 65536

type historySnapshot struct {
	token string
	data  []byte
	until time.Time
}

func (s *Service) tripHistoryReady() bool {
	if s.ipc == nil {
		return false
	}
	v, err := s.ipc.Get("trip:history:ready")
	return err == nil && v == "1"
}

func (s *Service) clearHistorySnapshot() {
	s.historyMu.Lock()
	s.historySnapshot = nil
	s.historyGeneration++
	s.historyMu.Unlock()
}

func (s *Service) handleHistoryCommand(cmd string) {
	if s.historyHandlers.Add(1) > 4 {
		s.historyHandlers.Add(-1)
		return
	}
	defer s.historyHandlers.Add(-1)
	parts := strings.Split(cmd, ":")
	s.historyMu.Lock()
	if len(parts) == 3 && parts[0] == "start" {
		s.historyGeneration++
		s.historySnapshot = nil
	}
	generation := s.historyGeneration
	response := "h:x:command"
	if len(parts) == 4 && parts[0] == "get" {
		response = historyChunk(s.historySnapshot, parts[1], parts[2], parts[3], time.Now())
	}
	s.historyMu.Unlock()
	if len(parts) == 3 && parts[0] == "start" {
		response = s.startHistorySnapshot(parts[1], parts[2], generation)
	}
	if response == "" {
		return
	}
	s.extRespMu.Lock()
	defer s.extRespMu.Unlock()
	s.waitExtendedResponseInterval()
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if generation == s.historyGeneration {
		s.writeExtendedResponse(response)
	}
}

func (s *Service) startHistorySnapshot(kind, encoded string, generation uint64) string {
	methods := map[string]string{"l": "history.list", "d": "history.get", "p": "history.points"}
	method, ok := methods[kind]
	if !ok || len(encoded) > 1368 {
		return "h:x:invalid"
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) > 1024 {
		return "h:x:invalid"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil || object == nil {
		return "h:x:invalid"
	}
	if !s.tripHistoryReady() || s.destIPC == nil {
		return "h:x:unavailable"
	}
	result, err := ipc.CallMethod[json.RawMessage, json.RawMessage](s.destIPC, "trip:history:rpc", method, json.RawMessage(payload), 3*time.Second)
	s.historyMu.Lock()
	current := generation == s.historyGeneration
	s.historyMu.Unlock()
	if !current {
		return ""
	}
	if err != nil {
		var callError *ipc.CallError
		code := "unavailable"
		if errors.As(err, &callError) {
			switch callError.Msg {
			case "trip not found":
				code = "not-found"
			case "invalid cursor":
				code = "cursor"
			case "invalid request":
				code = "invalid"
			}
		}
		return "h:x:" + code
	}
	if len(result) > historyResponseLimit || !json.Valid(result) {
		return "h:x:internal"
	}
	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "h:x:internal"
	}
	token := hex.EncodeToString(random[:])
	s.historyMu.Lock()
	if generation != s.historyGeneration {
		s.historyMu.Unlock()
		return ""
	}
	s.historySnapshot = &historySnapshot{token: token, data: append([]byte(nil), result...), until: time.Now().Add(2 * time.Minute)}
	s.historyMu.Unlock()
	return fmt.Sprintf("h:s:%s:%s", token, strconv.FormatInt(int64(len(result)), 36))
}

func historyChunk(snapshot *historySnapshot, token, offsetText, budgetText string, now time.Time) string {
	if snapshot == nil || snapshot.token != token || now.After(snapshot.until) {
		return "h:x:session"
	}
	offset, err := strconv.ParseUint(offsetText, 36, 32)
	if err != nil || int(offset) > len(snapshot.data) {
		return "h:x:offset"
	}
	budget, err := strconv.Atoi(budgetText)
	if err != nil || budget < 20 || budget > 244 {
		return "h:x:budget"
	}
	snapshot.until = now.Add(2 * time.Minute)
	if int(offset) == len(snapshot.data) {
		return fmt.Sprintf("h:e:%s:%s", token, offsetText)
	}
	prefix := fmt.Sprintf("h:p:%s:%s:", token, offsetText)
	remaining := snapshot.data[offset:]
	n := len(remaining)
	if n > budget {
		n = budget
	}
	for n > 0 && len(prefix)+base64.RawURLEncoding.EncodedLen(n) > budget {
		n--
	}
	if n == 0 {
		return "h:x:budget"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(remaining[:n])
}

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	hashValueMaxBytes = 256
	hashJSONMaxBytes  = 4096
	hashSnapshotLimit = 4
	hashHandlerLimit  = 4
	hashSnapshotTTL   = 5 * time.Minute
)

// Exact hash/field pairs are the entire read authority. Never derive this from
// a request, settings schema, Redis key discovery, or a hash-wide read.
var hashReadAllowlist = map[string]map[string]bool{
	"battery:0": {"present": true, "serial-number": true},
	"battery:1": {"present": true, "serial-number": true},
}

type hashReadRequest struct {
	operation string
	id        string
	budget    int
	hash      string
	fields    []string
	offset    int
}

type hashSnapshot struct {
	data  []byte
	until time.Time
}

func hashRequestID(text string) bool {
	if len(text) != 8 || text == "00000000" {
		return false
	}
	for _, c := range text {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func parseHashRequest(command string) (hashReadRequest, string) {
	req := hashReadRequest{id: "00000000"}
	parts := strings.SplitN(command, ":", 4)
	if len(parts) > 1 && hashRequestID(parts[1]) {
		req.id = parts[1]
	}
	if len(command)+len("hash:") > 100 || !utf8.ValidString(command) || len(parts) != 4 || req.id == "00000000" {
		return req, "command"
	}
	req.operation = parts[0]
	budgetText := parts[2]
	if req.operation == "get" {
		budgetText = parts[3]
	} else if req.operation != "read" {
		return req, "command"
	}
	budget, err := strconv.Atoi(budgetText)
	if err != nil || budget < 20 || budget > 244 || strconv.Itoa(budget) != budgetText {
		return req, "budget"
	}
	req.budget = budget
	if req.operation == "get" {
		offset, err := strconv.ParseUint(parts[2], 36, 16)
		if err != nil || offset > hashJSONMaxBytes || strconv.FormatUint(offset, 36) != parts[2] {
			return req, "offset"
		}
		req.offset = int(offset)
		return req, ""
	}
	hash, fieldsText, ok := strings.Cut(parts[3], "/")
	if !ok || hash == "" || fieldsText == "" {
		return req, "command"
	}
	req.hash = hash
	req.fields = strings.Split(fieldsText, ",")
	if len(req.fields) > 2 {
		return req, "command"
	}
	allowed, ok := hashReadAllowlist[hash]
	if !ok {
		return req, "denied"
	}
	for i, field := range req.fields {
		if field == "" || i > 0 && field == req.fields[0] {
			return req, "command"
		}
		if !allowed[field] {
			return req, "denied"
		}
	}
	return req, ""
}

// Length checks and HMGET run atomically, bounding the reply before any values
// leave Redis. Only this fixed read-only script is exposed, never client code.
const hashReadScript = `
for i = 2, #ARGV do
    if redis.call('HSTRLEN', KEYS[1], ARGV[i]) > tonumber(ARGV[1]) then
        return -1
    end
end
return redis.call('HMGET', KEYS[1], unpack(ARGV, 2))
`

func (s *Service) readHashValues(req hashReadRequest) ([]byte, string) {
	if s.ipc == nil {
		return nil, "redis"
	}
	args := []interface{}{hashValueMaxBytes}
	for _, field := range req.fields {
		args = append(args, field)
	}
	ctx, cancel := context.WithTimeout(s.ipc.Context(), 5*time.Second)
	defer cancel()
	result, err := s.ipc.Raw().Eval(ctx, hashReadScript, []string{req.hash}, args...).Result()
	if err != nil {
		return nil, "redis"
	}
	if result == int64(-1) {
		return nil, "large"
	}
	values, ok := result.([]interface{})
	if !ok || len(values) != len(req.fields) {
		return nil, "redis"
	}
	for _, value := range values {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return nil, "redis"
		}
		if len(text) > hashValueMaxBytes {
			return nil, "large"
		}
		if !utf8.ValidString(text) {
			return nil, "utf8"
		}
	}
	data, err := json.Marshal(values)
	if err != nil {
		return nil, "redis"
	}
	if len(data) > hashJSONMaxBytes {
		return nil, "large"
	}
	return data, ""
}

func (s *Service) clearHashSnapshots() {
	s.hashMu.Lock()
	s.hashSnapshots = nil
	s.hashGeneration++
	s.hashMu.Unlock()
}

func (s *Service) handleHashCommand(command string) {
	// The paced sender can block. Drop overload rather than accumulate response
	// waiters, including busy/error responses, behind the shared send mutex.
	if s.hashHandlers.Add(1) > hashHandlerLimit {
		s.hashHandlers.Add(-1)
		return
	}
	defer s.hashHandlers.Add(-1)
	generation := s.hashConnectionGeneration()
	if response := s.hashResponseForGeneration(command, generation); response != "" {
		s.extRespMu.Lock()
		defer s.extRespMu.Unlock()
		s.waitExtendedResponseInterval()
		// Disconnect must invalidate queued responses, including EOF after its
		// snapshot was removed. Keep the generation stable through transmission.
		s.hashMu.Lock()
		defer s.hashMu.Unlock()
		if generation == s.hashGeneration {
			s.writeExtendedResponse(response)
		}
	}
}

func (s *Service) hashConnectionGeneration() uint64 {
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	return s.hashGeneration
}

func (s *Service) hashResponse(command string) string {
	return s.hashResponseForGeneration(command, s.hashConnectionGeneration())
}

func (s *Service) hashResponseForGeneration(command string, generation uint64) string {
	req, code := parseHashRequest(command)
	failure := func(code string) string { return "r:x:" + req.id + ":" + code }
	if code != "" {
		return failure(code)
	}
	s.hashMu.Lock()
	if generation != s.hashGeneration {
		s.hashMu.Unlock()
		return ""
	}
	now := time.Now()
	for id, snapshot := range s.hashSnapshots {
		if !now.Before(snapshot.until) {
			delete(s.hashSnapshots, id)
		}
	}
	if req.operation == "get" {
		defer s.hashMu.Unlock()
		snapshot := s.hashSnapshots[req.id]
		if snapshot == nil || snapshot.data == nil {
			return failure("session")
		}
		response := hashChunk(snapshot.data, req)
		if req.offset == len(snapshot.data) {
			delete(s.hashSnapshots, req.id)
		}
		return response
	}
	if s.hashSnapshots[req.id] != nil {
		s.hashMu.Unlock()
		return failure("id")
	}
	if len(s.hashSnapshots) >= hashSnapshotLimit {
		s.hashMu.Unlock()
		return failure("busy")
	}
	if s.hashSnapshots == nil {
		s.hashSnapshots = make(map[string]*hashSnapshot)
	}
	snapshot := &hashSnapshot{until: now.Add(hashSnapshotTTL)}
	s.hashSnapshots[req.id] = snapshot
	s.hashMu.Unlock()

	data, code := s.readHashValues(req)
	s.hashMu.Lock()
	defer s.hashMu.Unlock()
	// Pointer identity also rejects a read completing after disconnect or expiry.
	if s.hashSnapshots[req.id] != snapshot {
		return ""
	}
	if code != "" {
		delete(s.hashSnapshots, req.id)
		return failure(code)
	}
	snapshot.data = data
	return fmt.Sprintf("r:s:%s:%s", req.id, strconv.FormatInt(int64(len(data)), 36))
}

func hashChunk(data []byte, req hashReadRequest) string {
	offset := strconv.FormatInt(int64(req.offset), 36)
	if req.offset > len(data) {
		return "r:x:" + req.id + ":offset"
	}
	if req.offset == len(data) {
		return "r:e:" + req.id + ":" + offset
	}
	prefix := "r:p:" + req.id + ":" + offset + ":"
	n := min(len(data)-req.offset, req.budget)
	for n > 0 && len(prefix)+base64.RawURLEncoding.EncodedLen(n) > req.budget {
		n--
	}
	return prefix + base64.RawURLEncoding.EncodeToString(data[req.offset:req.offset+n])
}

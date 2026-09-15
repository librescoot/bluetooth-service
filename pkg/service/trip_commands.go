package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const (
	tripCounterKey       = "trip:counter"
	tripCommandResultKey = "trip:command-result"
	tripCommandQueue     = "scooter:trip"
	tripResponseMaxBytes = 480
	tripResetIDBytes     = 16
)

var (
	tripResetTimeout = 15 * time.Second
	// tripResetRetryWindow mirrors trip-service's durable command-ID retention.
	tripResetRetryWindow = 30 * 24 * time.Hour
)

type tripResetRequest struct {
	ID        string `json:"id"`
	Op        string `json:"op"`
	Source    string `json:"source"`
	ExpiresAt int64  `json:"expires-at"`
}

type tripCommandResult struct {
	ID     string `json:"id"`
	Op     string `json:"op"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func encodeTripResetRequest(request tripResetRequest) ([]byte, error) {
	return json.Marshal(request)
}

func (s *Service) handleTripCommand(cmd string) {
	switch strings.TrimSpace(cmd) {
	case "get":
		s.handleTripGet()
	case "reset":
		s.handleTripReset()
	default:
		s.sendExtendedResponse("trip:error:unknown command")
	}
}

func (s *Service) tripServiceReady() bool {
	ready, err := s.ipc.Get("trip:ready")
	return err == nil && ready == "1"
}

func (s *Service) handleTripGet() {
	if !s.tripServiceReady() {
		s.sendExtendedResponse("trip:data:error:unavailable")
		return
	}
	fields, err := s.ipc.HGetAll(tripCounterKey)
	if err != nil {
		s.log.Warnf("trip counter unavailable: %v", err)
		s.sendExtendedResponse("trip:data:error:unavailable")
		return
	}

	response, reason := tripSnapshotResponse(fields)
	if reason != "" {
		s.sendExtendedResponse("trip:data:error:" + reason)
		return
	}
	s.sendExtendedResponse(response)
}

// tripSnapshotResponse converts the complete Redis projection into the compact
// Bluetooth response. It deliberately accepts no fields beyond the counter API.
func tripSnapshotResponse(fields map[string]string) (string, string) {
	if fields["api-version"] != "1" {
		return "", "unavailable"
	}

	parse := func(name string) (int64, bool) {
		value, err := parseTripInteger(fields[name])
		return value, err == nil
	}
	distance, ok := parse("distance-m")
	if !ok {
		return "", "invalid"
	}
	duration, ok := parse("duration-s")
	if !ok {
		return "", "invalid"
	}
	averageSpeed, ok := parse("average-speed-kmh")
	if !ok {
		return "", "invalid"
	}
	resetAt, ok := parse("reset-at")
	if !ok {
		return "", "invalid"
	}
	generation, ok := parse("generation")
	if !ok {
		return "", "invalid"
	}

	policy := fields["reset-policy"]
	if policy != "ride" && policy != "day" && policy != "battery" && policy != "manual" {
		return "", "invalid"
	}
	status := fields["status"]
	if status != "idle" && status != "recording" {
		return "", "invalid"
	}
	reason := fields["reset-reason"]
	if reason != "initial" && reason != "ride" && reason != "day" && reason != "battery" && reason != "manual" {
		return "", "invalid"
	}

	response := strings.Join([]string{
		"trip", "data",
		"distance-m", strconv.FormatInt(distance, 10),
		"duration-s", strconv.FormatInt(duration, 10),
		"average-speed-kmh", strconv.FormatInt(averageSpeed, 10),
		"reset-policy", policy,
		"reset-at", strconv.FormatInt(resetAt, 10),
		"reset-reason", reason,
		"generation", strconv.FormatInt(generation, 10),
		"status", status,
	}, ":")
	if len(response) > tripResponseMaxBytes {
		return "", "invalid"
	}
	return response, ""
}

func parseTripInteger(value string) (int64, error) {
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseInt(value, 10, 64)
}

func (s *Service) handleTripReset() {
	if !s.tripServiceReady() {
		s.sendExtendedResponse("trip:reset:error:unavailable")
		return
	}
	if err := s.ensureTripResultSubscription(); err != nil {
		s.log.Warnf("trip result subscription unavailable: %v", err)
		s.sendExtendedResponse("trip:reset:error:unavailable")
		return
	}

	id := s.retryTripResetID()
	if id == "" {
		var err error
		id, err = newTripResetID()
		if err != nil {
			s.log.Errorf("failed to generate trip reset ID: %v", err)
			s.sendExtendedResponse("trip:reset:error:internal")
			return
		}
	}
	resultCh, connectionGeneration, ok := s.registerTripReset(id)
	if !ok {
		s.sendExtendedResponse("trip:reset:error:busy")
		return
	}

	request := tripResetRequest{
		ID: id, Op: "counter.reset", Source: "bluetooth",
		ExpiresAt: time.Now().Add(tripResetTimeout).UnixMilli(),
	}
	payload, err := encodeTripResetRequest(request)
	if err != nil {
		s.removeTripReset(id)
		s.log.Errorf("failed to encode trip reset: %v", err)
		s.sendExtendedResponse("trip:reset:error:internal")
		return
	}
	if err := ipc.SendRequest(s.ipc, tripCommandQueue, payload); err != nil {
		s.removeTripReset(id)
		s.log.Warnf("failed to queue trip reset: %v", err)
		s.sendExtendedResponse("trip:reset:error:unavailable")
		return
	}

	go s.waitTripReset(id, connectionGeneration, resultCh)
}

func newTripResetID() (string, error) {
	bytes := make([]byte, tripResetIDBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "ble-" + hex.EncodeToString(bytes), nil
}

func (s *Service) ensureTripResultSubscription() error {
	s.tripResetMu.Lock()
	defer s.tripResetMu.Unlock()
	if s.tripResultSubscription != nil {
		return nil
	}

	sub, err := ipc.Subscribe[json.RawMessage](s.ipc, tripCommandResultKey, func(raw json.RawMessage) error {
		s.handleTripCommandResult(raw)
		return nil
	})
	if err != nil {
		return err
	}
	s.tripResultSubscription = sub
	return nil
}

func (s *Service) registerTripReset(id string) (chan tripCommandResult, uint64, bool) {
	s.tripResetMu.Lock()
	defer s.tripResetMu.Unlock()
	if len(s.tripResetPending) != 0 {
		return nil, 0, false
	}
	if s.tripResetPending == nil {
		s.tripResetPending = make(map[string]chan tripCommandResult)
	}
	resultCh := make(chan tripCommandResult, 1)
	s.tripResetPending[id] = resultCh
	return resultCh, s.tripResetConnectionGeneration, true
}

func (s *Service) retryTripResetID() string {
	s.tripResetMu.Lock()
	defer s.tripResetMu.Unlock()
	if s.tripResetRetryID == "" || !time.Now().Before(s.tripResetRetryUntil) {
		s.tripResetRetryID = ""
		s.tripResetRetryUntil = time.Time{}
		return ""
	}
	return s.tripResetRetryID
}

func (s *Service) retainTripResetRetry(id string, connectionGeneration uint64) {
	s.tripResetMu.Lock()
	defer s.tripResetMu.Unlock()
	if connectionGeneration != s.tripResetConnectionGeneration {
		return
	}
	s.tripResetRetryID = id
	s.tripResetRetryUntil = time.Now().Add(tripResetRetryWindow)
}

func (s *Service) noteTripResetBLEStatus(status string) {
	if !strings.EqualFold(strings.TrimSpace(status), "disconnected") {
		return
	}
	s.tripResetMu.Lock()
	s.tripResetConnectionGeneration++
	s.tripResetRetryID = ""
	s.tripResetRetryUntil = time.Time{}
	s.tripResetMu.Unlock()
}

func (s *Service) handleTripCommandResult(raw []byte) {
	var result tripCommandResult
	if err := json.Unmarshal(raw, &result); err != nil || result.ID == "" || result.Op != "counter.reset" {
		return
	}

	s.tripResetMu.Lock()
	resultCh, ok := s.tripResetPending[result.ID]
	if ok && s.tripResetRetryID == result.ID {
		s.tripResetRetryID = ""
		s.tripResetRetryUntil = time.Time{}
	}
	s.tripResetMu.Unlock()
	if !ok {
		return
	}
	select {
	case resultCh <- result:
	default:
	}
}

func (s *Service) waitTripReset(id string, connectionGeneration uint64, resultCh <-chan tripCommandResult) {
	defer s.removeTripReset(id)
	timer := time.NewTimer(tripResetTimeout)
	defer timer.Stop()

	select {
	case result := <-resultCh:
		s.sendExtendedResponse(tripResetResponse(result))
	case <-timer.C:
		s.retainTripResetRetry(id, connectionGeneration)
		s.sendExtendedResponse("trip:reset:error:timeout")
	case <-s.stopCh:
	}
}

func tripResetResponse(result tripCommandResult) string {
	if result.Status == "ok" {
		return "trip:reset:ok"
	}
	message := strings.ToLower(result.Status + " " + result.Error)
	switch {
	case strings.Contains(message, "busy"):
		return "trip:reset:error:busy"
	case strings.Contains(message, "invalid"):
		return "trip:reset:error:invalid"
	case strings.Contains(message, "unavailable"):
		return "trip:reset:error:unavailable"
	case strings.Contains(message, "timeout"):
		return "trip:reset:error:timeout"
	default:
		return "trip:reset:error:internal"
	}
}

func (s *Service) removeTripReset(id string) {
	s.tripResetMu.Lock()
	delete(s.tripResetPending, id)
	s.tripResetMu.Unlock()
}

func (s *Service) stopTripResetBridge() {
	s.tripResetMu.Lock()
	s.tripResetPending = nil
	s.tripResetRetryID = ""
	s.tripResetRetryUntil = time.Time{}
	sub := s.tripResultSubscription
	s.tripResultSubscription = nil
	s.tripResetMu.Unlock()
	if sub != nil {
		if err := sub.Unsubscribe(); err != nil {
			s.log.Warnf("failed to unsubscribe trip command results: %v", err)
		}
	}
}

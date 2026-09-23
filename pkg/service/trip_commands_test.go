package service

import (
	"testing"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

func validTripCounterFields() map[string]string {
	return map[string]string{
		"api-version":       "1",
		"distance-m":        "1234",
		"duration-s":        "567",
		"average-speed-kmh": "8",
		"reset-policy":      "ride",
		"reset-at":          "1700000000",
		"reset-reason":      "ride",
		"generation":        "3",
		"status":            "recording",
	}
}

func TestCapabilityRegistryForIsDeterministicAndDynamic(t *testing.T) {
	withoutDynamic := capabilityRegistryFor(false, false, false)
	const wantWithoutDynamic = "cap:ext:nav=2:keycard:usb:service-mode:time:config:status:alarm:ltc:pm:dbc:ota:settings"
	if withoutDynamic != wantWithoutDynamic {
		t.Fatalf("registry = %q, want %q", withoutDynamic, wantWithoutDynamic)
	}
	withDynamic := capabilityRegistryFor(true, true, true)
	const wantWithDynamic = "cap:ext:nav=2:keycard=2:usb:service-mode:time:config:status:alarm:ltc:ble:pm:dbc:ota:settings:trip"
	if withDynamic != wantWithDynamic {
		t.Fatalf("registry = %q, want %q", withDynamic, wantWithDynamic)
	}
	if len(withDynamic) > tripResponseMaxBytes {
		t.Fatalf("registry length = %d, limit %d", len(withDynamic), tripResponseMaxBytes)
	}
}

func TestTripSnapshotResponseValidatesAndBoundsData(t *testing.T) {
	response, reason := tripSnapshotResponse(validTripCounterFields())
	if reason != "" {
		t.Fatalf("valid snapshot rejected: %s", reason)
	}
	if len(response) > tripResponseMaxBytes {
		t.Fatalf("response length = %d, limit %d", len(response), tripResponseMaxBytes)
	}
	const want = "trip:data:distance-m:1234:duration-s:567:average-speed-kmh:8:reset-policy:ride:reset-at:1700000000:reset-reason:ride:generation:3:status:recording"
	if response != want {
		t.Fatalf("response = %q, want %q", response, want)
	}

	invalid := validTripCounterFields()
	invalid["duration-s"] = "not-a-number"
	if _, reason := tripSnapshotResponse(invalid); reason != "invalid" {
		t.Fatalf("invalid integer reason = %q, want invalid", reason)
	}

	unavailable := validTripCounterFields()
	unavailable["api-version"] = "2"
	if _, reason := tripSnapshotResponse(unavailable); reason != "unavailable" {
		t.Fatalf("api version reason = %q, want unavailable", reason)
	}

	for _, resetReason := range []string{"initial", "ride", "day", "battery", "manual"} {
		fields := validTripCounterFields()
		fields["reset-reason"] = resetReason
		if _, reason := tripSnapshotResponse(fields); reason != "" {
			t.Errorf("reset reason %q rejected: %s", resetReason, reason)
		}
	}
	invalidReason := validTripCounterFields()
	invalidReason["reset-reason"] = "manual:bluetooth"
	if _, reason := tripSnapshotResponse(invalidReason); reason != "invalid" {
		t.Fatalf("invalid reset reason = %q, want invalid", reason)
	}

	bounded := validTripCounterFields()
	bounded["distance-m"] = "9223372036854775807"
	bounded["duration-s"] = "9223372036854775807"
	bounded["average-speed-kmh"] = "9223372036854775807"
	bounded["reset-at"] = "9223372036854775807"
	bounded["generation"] = "9223372036854775807"
	response, reason = tripSnapshotResponse(bounded)
	if reason != "" || len(response) > tripResponseMaxBytes {
		t.Fatalf("bounded response = %q (%d bytes), reason %q", response, len(response), reason)
	}
}

func TestTripCounterCapabilityRequiresReadySchema(t *testing.T) {
	validSchema := map[string]settingSchema{
		"trip.counter-reset": {
			Type: "enum",
			Values: []settingSchemaValue{
				{Value: "ride"}, {Value: "day"}, {Value: "battery"}, {Value: "manual"},
			},
		},
	}
	if !tripCounterCapabilitySupported("1", "1", validSchema) {
		t.Fatal("ready trip counter with writable current schema was omitted")
	}
	if tripCounterCapabilitySupported("1", "", validSchema) {
		t.Fatal("trip capability accepted missing readiness key")
	}
	if tripCounterCapabilitySupported("2", "1", validSchema) {
		t.Fatal("trip capability accepted unsupported API version")
	}
	readOnlySchema := map[string]settingSchema{
		"trip.counter-reset": {
			Type:     "enum",
			ReadOnly: true,
			Values:   validSchema["trip.counter-reset"].Values,
		},
	}
	if tripCounterCapabilitySupported("1", "1", readOnlySchema) {
		t.Fatal("trip capability accepted read-only setting")
	}
	missingValueSchema := map[string]settingSchema{
		"trip.counter-reset": {
			Type: "enum",
			Values: []settingSchemaValue{
				{Value: "ride"}, {Value: "day"}, {Value: "battery"}, {Value: "other"},
			},
		},
	}
	if tripCounterCapabilitySupported("1", "1", missingValueSchema) {
		t.Fatal("trip capability accepted incomplete enum")
	}
}

func TestTripResetRequestEncodingUsesStringCodecPayload(t *testing.T) {
	payload, err := encodeTripResetRequest(tripResetRequest{
		ID: "ble-test", Op: "counter.reset", Source: "bluetooth", ExpiresAt: 1234,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"id":"ble-test","op":"counter.reset","source":"bluetooth","expires-at":1234}`
	if string(payload) != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

func TestTripResetResultCorrelationAndStaleResults(t *testing.T) {
	s := &Service{tripResetPending: make(map[string]chan tripCommandResult)}
	resultCh, _, ok := s.registerTripReset("ble-current")
	if !ok {
		t.Fatal("registerTripReset rejected first request")
	}

	s.handleTripCommandResult([]byte(`{"id":"ble-stale","op":"counter.reset","status":"ok"}`))
	select {
	case result := <-resultCh:
		t.Fatalf("stale result delivered: %+v", result)
	default:
	}

	s.handleTripCommandResult([]byte(`{"id":"ble-current","op":"counter.reset","status":"ok"}`))
	select {
	case result := <-resultCh:
		if result.ID != "ble-current" || result.Status != "ok" {
			t.Fatalf("result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("matching result was not delivered")
	}
}

func TestTripResetPendingSerializesAndMapsErrors(t *testing.T) {
	s := &Service{}
	if _, _, ok := s.registerTripReset("ble-one"); !ok {
		t.Fatal("first reset was rejected")
	}
	if _, _, ok := s.registerTripReset("ble-two"); ok {
		t.Fatal("second reset was not rejected as busy")
	}
	if got := tripResetResponse(tripCommandResult{Status: "error", Error: "counter is busy while ready-to-drive"}); got != "trip:reset:error:busy" {
		t.Fatalf("busy response = %q", got)
	}
	if got := tripResetResponse(tripCommandResult{Status: "error", Error: "invalid command"}); got != "trip:reset:error:invalid" {
		t.Fatalf("invalid response = %q", got)
	}
	if got := tripResetResponse(tripCommandResult{Status: "error", Error: "request timeout"}); got != "trip:reset:error:timeout" {
		t.Fatalf("timeout response = %q", got)
	}
	if got := tripResetResponse(tripCommandResult{Status: "error", Error: "database failed"}); got != "trip:reset:error:internal" {
		t.Fatalf("internal response = %q", got)
	}
}

func TestTripResetRetryIDIsScopedToOneConnection(t *testing.T) {
	s := &Service{}
	s.retainTripResetRetry("ble-ambiguous", 0)
	if got := s.retryTripResetID(); got != "ble-ambiguous" {
		t.Fatalf("retry ID = %q, want ble-ambiguous", got)
	}

	s.noteTripResetBLEStatus("connected")
	if got := s.retryTripResetID(); got != "ble-ambiguous" {
		t.Fatalf("connected status cleared retry ID = %q", got)
	}

	s.noteTripResetBLEStatus("disconnected")
	if got := s.retryTripResetID(); got != "" {
		t.Fatalf("disconnected status left retry ID = %q", got)
	}

	s.retainTripResetRetry("ble-stale", 0)
	if got := s.retryTripResetID(); got != "" {
		t.Fatalf("old connection retained retry ID = %q", got)
	}

	_, generation, ok := s.registerTripReset("ble-current")
	if !ok || generation != 1 {
		t.Fatalf("connection generation = %d, registered = %v", generation, ok)
	}
	s.removeTripReset("ble-current")
	s.retainTripResetRetry("ble-current", generation)
	if got := s.retryTripResetID(); got != "ble-current" {
		t.Fatalf("current connection retry ID = %q", got)
	}
}

func TestTripResetTimeoutAndShutdownCleanup(t *testing.T) {
	oldTimeout := tripResetTimeout
	tripResetTimeout = time.Millisecond
	defer func() { tripResetTimeout = oldTimeout }()

	s := &Service{stopCh: make(chan struct{}), log: logger.NewLogger(nil, logger.LogLevelNone)}
	timeoutCh, timeoutGeneration, ok := s.registerTripReset("ble-timeout")
	if !ok {
		t.Fatal("registerTripReset rejected timeout request")
	}
	go s.waitTripReset("ble-timeout", timeoutGeneration, timeoutCh)
	waitForTripResetCleanup(t, s, "ble-timeout")
	if got := s.retryTripResetID(); got != "ble-timeout" {
		t.Fatalf("timeout retry ID = %q, want ble-timeout", got)
	}

	shutdownCh, shutdownGeneration, ok := s.registerTripReset("ble-shutdown")
	if !ok {
		t.Fatal("registerTripReset rejected shutdown request")
	}
	go s.waitTripReset("ble-shutdown", shutdownGeneration, shutdownCh)
	close(s.stopCh)
	waitForTripResetCleanup(t, s, "ble-shutdown")

	_, _, ok = s.registerTripReset("ble-bridge")
	if !ok {
		t.Fatal("registerTripReset rejected bridge request")
	}
	s.stopTripResetBridge()
	s.tripResetMu.Lock()
	pending := len(s.tripResetPending)
	s.tripResetMu.Unlock()
	if pending != 0 {
		t.Fatalf("shutdown left %d pending resets", pending)
	}
}

func waitForTripResetCleanup(t *testing.T, s *Service, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.tripResetMu.Lock()
		_, pending := s.tripResetPending[id]
		s.tripResetMu.Unlock()
		if !pending {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending reset %q was not cleaned up", id)
}

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/librescoot/bluetooth-service/pkg/ble"
	"github.com/librescoot/bluetooth-service/pkg/logger"
)

func TestPairingResponseWhitelistsAndBoundsPublicData(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"state": "waiting_for_web_setup", "running": true, "code": "01234-56789", "url": "https://attacker.invalid/private-qr", "expires_at": time.Now().Add(time.Minute).Unix(), "registration": map[string]string{"token": "private-poll-token"}, "claim_token": "private-provider-token", "error": "private-error"})
	response, err := pairingResponse(data)
	if err != nil || len(response) > 244 || !strings.Contains(response, "01234-56789") || !strings.Contains(response, "https://connect.librescoot.org/pair") || strings.Contains(response, "private-") || strings.Contains(response, "attacker") {
		t.Fatal("unsafe pairing response")
	}
	for _, data := range []string{`{"state":"private-state"}`, strings.Repeat("x", 4097), `invalid`} {
		if _, err := pairingResponse([]byte(data)); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	response, err = pairingResponse([]byte(`{"state":"waiting_for_web_setup","running":true,"code":"01234-56789","expires_at":1}`))
	if err != nil || strings.Contains(response, "01234-56789") {
		t.Fatal("expired code exposed")
	}
}

func TestPairingExtendedDispatcherIsAsyncAndSingleFlight(t *testing.T) {
	sock := &mockUSOCK{}
	entered, release := make(chan string, 1), make(chan struct{})
	s := &Service{usock: sock, log: logger.NewLogger(nil, logger.LogLevelNone), pairingRun: func(ctx context.Context, action string) ([]byte, error) {
		entered <- action
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []byte(`{"state":"starting","running":true}`), nil
	}}
	s.handleExtendedCommandMessage(ble.TypeExtended, 0, "cloud-pairing:start")
	select {
	case action := <-entered:
		if action != "start" {
			t.Fatal("wrong action")
		}
	case <-time.After(time.Second):
		t.Fatal("worker not started")
	}
	s.handlePairingCommand("status")
	if got := routeResponse(t, sock); got != "cloud-pairing:error:busy" {
		t.Fatalf("got %q", got)
	}
	close(release)
	s.pairingWG.Wait()
	if got := routeResponse(t, sock); !strings.HasPrefix(got, "cloud-pairing:status:") {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(capabilityRegistryFor(false, false), "cloud-pairing=1") || len(capabilityMap["cloud-pairing"]) != 3 {
		t.Fatal("missing capability")
	}
}

func TestPairingRejectsArgumentsAndRedactsRunnerErrors(t *testing.T) {
	sock := &mockUSOCK{}
	calls := 0
	s := &Service{usock: sock, log: logger.NewLogger(nil, logger.LogLevelNone), pairingRun: func(context.Context, string) ([]byte, error) {
		calls++
		return nil, errors.New("private-provider-token")
	}}
	s.handlePairingCommand("start;touch /tmp/unsafe")
	if calls != 0 || routeResponse(t, sock) != "cloud-pairing:error:unknown-command" {
		t.Fatal("arbitrary command accepted")
	}
	s.handlePairingCommand("status")
	s.pairingWG.Wait()
	if got := routeResponse(t, sock); got != "cloud-pairing:error:unavailable" || strings.Contains(got, "private-") {
		t.Fatal("runner error escaped")
	}
}

func TestPairingShutdownCancelsHandlerWithoutCancellingApprovedWorker(t *testing.T) {
	sock := &mockUSOCK{}
	entered := make(chan struct{})
	s := &Service{usock: sock, log: logger.NewLogger(nil, logger.LogLevelNone), pairingRun: func(ctx context.Context, _ string) ([]byte, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s.handlePairingCommand("status")
	<-entered
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stopPairingCommands()
	if sock.messageCount() != 0 {
		t.Fatal("response emitted during shutdown")
	}
	s.handlePairingCommand("start")
	if s.pairingBusy {
		t.Fatal("new request accepted after shutdown")
	}
}

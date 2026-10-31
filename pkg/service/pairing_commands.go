package service

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"time"
)

type pairingSnapshot struct {
	State     string `json:"state"`
	Running   bool   `json:"running"`
	Code      string `json:"code,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

var pairingDisplayCode = regexp.MustCompile(`^[0-9ABCDEFGHJKMNPQRSTVWXYZ]{5}-[0-9ABCDEFGHJKMNPQRSTVWXYZ]{5}$`)

func pairingResponse(data []byte) (string, error) {
	var snapshot pairingSnapshot
	if len(data) > 4096 || json.Unmarshal(data, &snapshot) != nil {
		return "", errors.New("invalid pairing response")
	}
	switch snapshot.State {
	case "not_registered", "registered", "starting", "waiting_for_web_setup", "applying", "connected", "error", "stopping", "paused":
	default:
		return "", errors.New("invalid pairing state")
	}
	if snapshot.State != "waiting_for_web_setup" || !pairingDisplayCode.MatchString(snapshot.Code) || snapshot.ExpiresAt <= time.Now().Unix() {
		snapshot.Code = ""
		snapshot.URL = ""
		snapshot.ExpiresAt = 0
	} else {
		snapshot.URL = "https://connect.librescoot.org/pair"
	}
	output, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	response := "cloud-pairing:status:" + string(output)
	if len(response) > 244 {
		return "", errors.New("pairing response too large")
	}
	return response, nil
}

func runPairingCLI(ctx context.Context, action string) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/connect", "-json", action).Output()
}

func (s *Service) handlePairingCommand(action string) {
	switch action {
	case "start", "status", "stop":
	default:
		s.sendExtendedResponse("cloud-pairing:error:unknown-command")
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if s.pairingBusy {
		s.mu.Unlock()
		s.sendExtendedResponse("cloud-pairing:error:busy")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	s.pairingBusy = true
	s.pairingCancel = cancel
	s.pairingWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.pairingWG.Done()
		defer cancel()
		defer func() { s.mu.Lock(); s.pairingBusy = false; s.pairingCancel = nil; s.mu.Unlock() }()
		run := s.pairingRun
		if run == nil {
			run = runPairingCLI
		}
		data, err := run(ctx, action)
		response := "cloud-pairing:error:unavailable"
		if err == nil {
			if value, parseErr := pairingResponse(data); parseErr == nil {
				response = value
			}
		}
		s.mu.RLock()
		stopped := s.stopped
		s.mu.RUnlock()
		if !stopped {
			s.sendExtendedResponse(response)
		}
	}()
}

func (s *Service) stopPairingCommands() {
	s.mu.Lock()
	if s.pairingCancel != nil {
		s.pairingCancel()
	}
	s.mu.Unlock()
	s.pairingWG.Wait()
}

package service

import (
	"encoding/json"
	"errors"
	ipc "github.com/librescoot/redis-ipc"
	"time"
)

const lockCallTimeout = 3 * time.Second

type lockCall func(command string, deadline time.Time) (string, error)

func (s *Service) callVehicleLock(command string, deadline time.Time) (string, error) {
	payload, err := json.Marshal(struct {
		Version  int    `json:"version"`
		Command  string `json:"command"`
		Deadline int64  `json:"deadline"`
	}{1, command, deadline.UnixMilli()})
	if err != nil {
		return "", err
	}
	return ipc.Call[string, string](s.ipc, "scooter:lock", string(payload), time.Until(deadline))
}

func lockCapabilityResponses(call lockCall) []string {
	response, err := call("capabilities", time.Now().Add(lockCallTimeout))
	if err != nil && !errors.Is(err, ipc.ErrCallTimeout) && !ipc.IsCallError(err) {
		return []string{"cap:lock:error:redis"}
	}
	var token string
	if err != nil || json.Unmarshal([]byte(response), &token) != nil || token != "lock:v1:ignore-seatbox" {
		return []string{"cap:lock:count:0"}
	}
	return []string{"cap:lock:count:1", "cap:lock:ignore-seatbox"}
}

func lockOverrideResponse(command string, call lockCall) string {
	if command != "ignore-seatbox" {
		return "lock:error:invalid"
	}
	response, err := call(command, time.Now().Add(lockCallTimeout))
	if err != nil {
		var remote *ipc.CallError
		if errors.As(err, &remote) {
			switch remote.Msg {
			case "unsupported", "invalid", "unsafe-state", "expired":
				return "lock:error:" + remote.Msg
			}
		}
		// Transport failure can occur after dispatch/acceptance. Never infer a
		// definite non-write or retry; telemetry is the source of physical state.
		return "lock:error:unknown-outcome"
	}
	var token string
	if json.Unmarshal([]byte(response), &token) != nil || token != "lock:v1:accepted" {
		return "lock:error:unknown-outcome"
	}
	return "lock:accepted"
}

package service

import (
	"encoding/json"
	"errors"
	"fmt"
	ipc "github.com/librescoot/redis-ipc"
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestLockOverrideResponse(t *testing.T) {
	for _, tc := range []struct {
		command, reply string
		err            error
		want           string
		calls          int
	}{
		{"ignore-seatbox", `"lock:v1:accepted"`, nil, "lock:accepted", 1},
		{"ignore-seatbox", `"lock:v2:accepted"`, nil, "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", `"lock:v1:accepted" true`, nil, "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", `"accepted"`, nil, "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", "", ipc.ErrCallTimeout, "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", "", errors.New("redis down"), "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", "", &ipc.CallError{Msg: "unsafe-state"}, "lock:error:unsafe-state", 1},
		{"ignore-seatbox", "", &ipc.CallError{Msg: "unsupported"}, "lock:error:unsupported", 1},
		{"ignore-seatbox", "", &ipc.CallError{Msg: "processing"}, "lock:error:unknown-outcome", 1},
		{"ignore-seatbox", "", &ipc.CallError{Msg: "expired"}, "lock:error:expired", 1},
		{"ignore-seatbox:force", "", nil, "lock:error:invalid", 0},
		{"ignore-seatbox ", "", nil, "lock:error:invalid", 0},
		{"force-lock", "", nil, "lock:error:invalid", 0},
		{"", "", nil, "lock:error:invalid", 0},
	} {
		t.Run(tc.command+tc.want+tc.reply, func(t *testing.T) {
			calls := 0
			got := lockOverrideResponse(tc.command, func(command string, deadline time.Time) (string, error) {
				calls++
				if command != "ignore-seatbox" || !time.Now().Before(deadline) {
					t.Fatal("bad RPC")
				}
				return tc.reply, tc.err
			})
			if got != tc.want || calls != tc.calls {
				t.Fatalf("got %q calls %d, want %q calls %d", got, calls, tc.want, tc.calls)
			}
		})
	}
}
func TestLockOverrideCapabilities(t *testing.T) {
	for _, tc := range []struct {
		reply string
		err   error
		want  string
	}{
		{`"lock:v1:ignore-seatbox"`, nil, "cap:lock:count:1"},
		{`"lock:v2:ignore-seatbox"`, nil, "cap:lock:count:0"},
		{`"lock:v1:ignore-seatbox" {}`, nil, "cap:lock:count:0"},
		{"", ipc.ErrCallTimeout, "cap:lock:count:0"},
		{"", &ipc.CallError{Msg: "unsupported"}, "cap:lock:count:0"},
		{"", errors.New("redis down"), "cap:lock:error:redis"},
	} {
		t.Run(tc.want+tc.reply, func(t *testing.T) {
			got := lockCapabilityResponses(func(command string, deadline time.Time) (string, error) {
				if command != "capabilities" {
					t.Fatal("probe actuated")
				}
				return tc.reply, tc.err
			})
			if got[0] != tc.want {
				t.Fatalf("%v", got)
			}
			if tc.want == "cap:lock:count:1" && (len(got) != 2 || got[1] != "cap:lock:ignore-seatbox") {
				t.Fatal(got)
			}
		})
	}
}

func TestLockOverrideRedisWire(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	process := exec.Command(binary, "--bind", "127.0.0.1", "--port", strconv.Itoa(port), "--save", "", "--appendonly", "no", "--dir", t.TempDir())
	if err = process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill(); _ = process.Wait() })
	for end := time.Now().Add(time.Second); ; {
		conn, e := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 20*time.Millisecond)
		if e == nil {
			conn.Close()
			break
		}
		if time.Now().After(end) {
			t.Fatal("test Redis did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	client, err := ipc.New(ipc.WithAddress("127.0.0.1"), ipc.WithPort(port), ipc.WithCodec(ipc.StringCodec{}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	requests := make(chan string, 4)
	handler := ipc.HandleCalls(client, "scooter:lock", func(raw string) (string, error) {
		var req struct {
			Version  int    `json:"version"`
			Command  string `json:"command"`
			Deadline int64  `json:"deadline"`
		}
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			return "", err
		}
		if req.Version != 1 || req.Deadline <= time.Now().UnixMilli() {
			return "", fmt.Errorf("invalid")
		}
		requests <- req.Command
		if req.Command == "capabilities" {
			return `"lock:v1:ignore-seatbox"`, nil
		}
		return `"lock:v1:accepted"`, nil
	})
	defer handler.Stop()
	s := &Service{ipc: client}
	capabilities := lockCapabilityResponses(s.callVehicleLock)
	if len(capabilities) != 2 || capabilities[1] != "cap:lock:ignore-seatbox" {
		t.Fatal(capabilities)
	}
	if command := <-requests; command != "capabilities" {
		t.Fatalf("probe actuated: %q", command)
	}
	if reply := lockOverrideResponse("ignore-seatbox", s.callVehicleLock); reply != "lock:accepted" {
		t.Fatal(reply)
	}
	if command := <-requests; command != "ignore-seatbox" {
		t.Fatal(command)
	}
	handler.Stop()
	// Simulate an older vehicle with no handler; use a short bounded Call.
	if _, err = s.callVehicleLock("capabilities", time.Now().Add(30*time.Millisecond)); !errors.Is(err, ipc.ErrCallTimeout) {
		t.Fatal(err)
	}
	// Client failure is never success, and may follow an accepted dispatch.
	client.Close()
	if reply := lockOverrideResponse("ignore-seatbox", s.callVehicleLock); reply != "lock:error:unknown-outcome" {
		t.Fatal(reply)
	}
}

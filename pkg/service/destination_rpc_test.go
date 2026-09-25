package service

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	ipc "github.com/librescoot/redis-ipc"
	"github.com/redis/go-redis/v9"

	"github.com/librescoot/bluetooth-service/pkg/logger"
)

// destinationEnvelope mirrors the redis-ipc request envelope.
type destinationEnvelope struct {
	ID           string          `json:"id"`
	Method       string          `json:"method"`
	ReplyChannel string          `json:"reply_channel"`
	Deadline     int64           `json:"deadline"`
	Payload      json.RawMessage `json:"payload"`
}

// startDestinationServer plays settings-service's side of the wire: BRPOP the
// request queue, hand the envelope to the handler, publish its reply.
func startDestinationServer(t *testing.T, mr *miniredis.Miniredis,
	handler func(env destinationEnvelope) []byte) chan destinationEnvelope {
	t.Helper()
	return startCallServer(t, mr, destinationChannel, handler)
}

func startCallServer(t *testing.T, mr *miniredis.Miniredis, channel string, handler func(env destinationEnvelope) []byte) chan destinationEnvelope {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	envelopes := make(chan destinationEnvelope, 8)
	go func() {
		for {
			result, err := rdb.BRPop(ctx, 0, channel).Result()
			if err != nil {
				return
			}
			var env destinationEnvelope
			if err := json.Unmarshal([]byte(result[1]), &env); err != nil {
				continue
			}
			envelopes <- env
			if reply := handler(env); reply != nil {
				rdb.Publish(ctx, env.ReplyChannel, reply)
			}
		}
	}()
	return envelopes
}

func newDestinationService(t *testing.T, mr *miniredis.Miniredis) *Service {
	t.Helper()
	host, portStr, err := net.SplitHostPort(mr.Addr())
	if err != nil {
		t.Fatalf("split miniredis addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse miniredis port: %v", err)
	}
	client, err := ipc.New(ipc.WithAddress(host), ipc.WithPort(port))
	if err != nil {
		t.Fatalf("ipc.New: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return &Service{
		destIPC: client,
		log:     logger.NewLogger(nil, logger.LogLevelNone),
	}
}

func TestAddSavedLocationCallsDestinationSave(t *testing.T) {
	mr := miniredis.RunT(t)
	envelopes := startDestinationServer(t, mr, func(destinationEnvelope) []byte {
		return []byte(`{"ok":true,"payload":{"id":2,"uuid":"3fa85f64-5717-4562-b3fc-2c963f66afa6"}}`)
	})
	s := newDestinationService(t, mr)

	id, err := s.addSavedLocation(" 52.5 ", "13.4", "Home")
	if err != nil {
		t.Fatalf("addSavedLocation: %v", err)
	}
	if id != 2 {
		t.Errorf("id = %d, want 2 from the reply", id)
	}

	env := <-envelopes
	if env.Method != "destination.save" {
		t.Errorf("method = %q, want destination.save", env.Method)
	}
	if env.ID == "" {
		t.Error("envelope has no id")
	}
	if !strings.HasPrefix(env.ReplyChannel, destinationChannel+":reply:") {
		t.Errorf("reply channel = %q, want a per-call channel", env.ReplyChannel)
	}
	if env.Deadline <= time.Now().UnixMilli() {
		t.Errorf("deadline = %d, want a future timestamp", env.Deadline)
	}
	var request destinationSaveRequest
	if err := json.Unmarshal(env.Payload, &request); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if request.ID != nil {
		t.Errorf("id = %d, want absent so the server allocates", *request.ID)
	}
	if request.Latitude != 52.5 || request.Longitude != 13.4 || request.Label != "Home" {
		t.Errorf("request = %+v, want trimmed coordinates and label", request)
	}
}

func TestAddSavedLocationRejectsBadCoordinatesWithoutCalling(t *testing.T) {
	mr := miniredis.RunT(t)
	envelopes := startDestinationServer(t, mr, func(env destinationEnvelope) []byte {
		t.Errorf("unexpected call for %q", env.Method)
		return []byte(`{"ok":true,"payload":{"id":0,"uuid":""}}`)
	})
	s := newDestinationService(t, mr)

	if id, err := s.addSavedLocation("banana", "13.4", "Home"); err == nil || id != 0 {
		t.Errorf("addSavedLocation(banana) = (%d, %v), want (0, error)", id, err)
	}
	if id, err := s.addSavedLocation("52.5", "nope", "Home"); err == nil || id != 0 {
		t.Errorf("addSavedLocation(nope longitude) = (%d, %v), want (0, error)", id, err)
	}
	select {
	case env := <-envelopes:
		t.Errorf("RPC fired despite invalid input: %q", env.Method)
	default:
	}
}

func TestDeleteSavedLocationCallsDestinationDelete(t *testing.T) {
	mr := miniredis.RunT(t)
	envelopes := startDestinationServer(t, mr, func(destinationEnvelope) []byte {
		return []byte(`{"ok":true,"payload":{}}`)
	})
	s := newDestinationService(t, mr)

	if err := s.deleteSavedLocation("3"); err != nil {
		t.Fatalf("deleteSavedLocation: %v", err)
	}
	env := <-envelopes
	if env.Method != "destination.delete" {
		t.Errorf("method = %q, want destination.delete", env.Method)
	}
	var request destinationIDRequest
	if err := json.Unmarshal(env.Payload, &request); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if request.ID != 3 {
		t.Errorf("id = %d, want 3", request.ID)
	}
}

func TestDeleteSavedLocationSurfacesErrors(t *testing.T) {
	t.Run("server error", func(t *testing.T) {
		mr := miniredis.RunT(t)
		startDestinationServer(t, mr, func(destinationEnvelope) []byte {
			return []byte(`{"ok":false,"error":"boom"}`)
		})
		s := newDestinationService(t, mr)

		err := s.deleteSavedLocation("3")
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want the server error", err)
		}
	})

	t.Run("invalid id never reaches the wire", func(t *testing.T) {
		mr := miniredis.RunT(t)
		envelopes := startDestinationServer(t, mr, func(env destinationEnvelope) []byte {
			t.Errorf("unexpected call for %q", env.Method)
			return nil
		})
		s := newDestinationService(t, mr)

		if err := s.deleteSavedLocation("abc"); err == nil {
			t.Error("deleteSavedLocation(abc) succeeded")
		}
		select {
		case env := <-envelopes:
			t.Errorf("RPC fired despite invalid input: %q", env.Method)
		default:
		}
	})
}

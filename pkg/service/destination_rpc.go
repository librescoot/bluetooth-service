package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Destination records are managed by settings-service over the redis-ipc call
// channel. This service builds its ipc client with a string codec, so the
// calls carry hand-built JSON envelopes in the shape the CallServer
// dispatches: {id, method, reply_channel, deadline, payload} answered with
// {ok, payload, error} on the reply channel.

const destinationChannel = "settings:destinations"
const destinationCallTimeout = 5 * time.Second

type destinationSaveRequest struct {
	ID        *int    `json:"id,omitempty"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Label     string  `json:"label"`
}

type destinationSaveResponse struct {
	ID   int    `json:"id"`
	UUID string `json:"uuid"`
}

type destinationIDRequest struct {
	ID int `json:"id"`
}

type destinationEmptyResponse struct{}

type destinationReply struct {
	OK      bool            `json:"ok"`
	Payload json.RawMessage `json:"payload"`
	Error   string          `json:"error"`
}

func (s *Service) callDestination(method string, request, response any) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	callID, err := newDestinationCallID()
	if err != nil {
		return err
	}
	replyChannel := destinationChannel + ":reply:" + callID
	envelope, err := json.Marshal(map[string]any{
		"id":            callID,
		"method":        method,
		"reply_channel": replyChannel,
		"deadline":      time.Now().Add(destinationCallTimeout).UnixMilli(),
		"payload":       json.RawMessage(payload),
	})
	if err != nil {
		return fmt.Errorf("encode %s envelope: %w", method, err)
	}

	ctx, cancel := context.WithTimeout(s.ipc.Context(), destinationCallTimeout)
	defer cancel()
	rdb := s.ipc.Raw()

	sub := rdb.Subscribe(ctx, replyChannel)
	defer sub.Close()
	if _, err := sub.Receive(ctx); err != nil {
		return fmt.Errorf("subscribe %s reply: %w", method, err)
	}
	if err := rdb.LPush(ctx, destinationChannel, envelope).Err(); err != nil {
		return fmt.Errorf("queue %s call: %w", method, err)
	}
	msg, err := sub.ReceiveMessage(ctx)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s timed out", method)
		}
		return fmt.Errorf("receive %s reply: %w", method, err)
	}

	var reply destinationReply
	if err := json.Unmarshal([]byte(msg.Payload), &reply); err != nil {
		return fmt.Errorf("decode %s reply: %w", method, err)
	}
	if !reply.OK {
		return fmt.Errorf("%s failed: %s", method, reply.Error)
	}
	if err := json.Unmarshal(reply.Payload, response); err != nil {
		return fmt.Errorf("decode %s response: %w", method, err)
	}
	return nil
}

func newDestinationCallID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate call id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

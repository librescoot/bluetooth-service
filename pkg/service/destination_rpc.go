package service

import (
	"fmt"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

// Destination records are managed by settings-service over the redis-ipc call
// channel. Calls ride a JSON-codec client; the service's own client uses a
// string codec for its command payloads.

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

// destinationCall performs one typed RPC against settings-service.
func destinationCall[Req, Resp any](client *ipc.Client, method string, request Req,
	response *Resp) error {
	if client == nil {
		return fmt.Errorf("destination client not configured")
	}
	result, err := ipc.CallMethod[Req, Resp](client, destinationChannel, method,
		request, destinationCallTimeout)
	if err != nil {
		return err
	}
	*response = result
	return nil
}

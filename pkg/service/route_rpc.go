package service

import (
	"fmt"
	"time"

	ipc "github.com/librescoot/redis-ipc"
)

const routePlanChannel = "settings:route-plan"
const routeCallTimeout = 5 * time.Second

type routeStop struct {
	ID      string  `json:"id"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Label   string  `json:"label"`
	Reached bool    `json:"reached"`
}
type routePlan struct {
	ID          string      `json:"id"`
	Revision    uint64      `json:"revision"`
	Stops       []routeStop `json:"stops"`
	CurrentStep int         `json:"current_step"`
}
type routeAppendRequest struct {
	Stop navStop `json:"stop"`
}
type routeReplaceRequest struct {
	Stops []navStop `json:"stops"`
}
type routeRemoveRequest struct {
	Index            int    `json:"index"`
	ExpectedRevision uint64 `json:"expected_revision"`
}
type routeProgressRequest struct {
	ExpectedPlanID string `json:"expected_plan_id"`
	ExpectedStopID string `json:"expected_stop_id"`
}
type routeClearRequest struct{}
type routeEmptyRequest struct{}

func routeCall[Req, Resp any](client *ipc.Client, method string, request Req, response *Resp) error {
	if client == nil {
		return fmt.Errorf("route plan client not configured")
	}
	result, err := ipc.CallMethod[Req, Resp](client, routePlanChannel, method, request, routeCallTimeout)
	if err != nil {
		return err
	}
	*response = result
	return nil
}
func (s *Service) getRoutePlan() (routePlan, error) {
	var plan routePlan
	err := routeCall(s.destIPC, "plan.get", routeEmptyRequest{}, &plan)
	return plan, err
}
func (s *Service) replaceRoutePlan(stop navStop) error {
	var plan routePlan
	return routeCall(s.destIPC, "plan.replace", routeReplaceRequest{Stops: []navStop{stop}}, &plan)
}
func (s *Service) clearRoutePlan() error {
	var plan routePlan
	return routeCall(s.destIPC, "plan.clear", routeClearRequest{}, &plan)
}

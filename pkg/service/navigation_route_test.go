package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/fxamacker/cbor/v2"
)

func startRouteServer(t *testing.T, mr *miniredis.Miniredis, handler func(destinationEnvelope) []byte) chan destinationEnvelope {
	t.Helper()
	return startCallServer(t, mr, routePlanChannel, handler)
}

func routeReply(plan routePlan) []byte {
	data, _ := json.Marshal(plan)
	return []byte(fmt.Sprintf(`{"ok":true,"payload":%s}`, data))
}
func routeResponse(t *testing.T, sock *mockUSOCK) string {
	t.Helper()
	var payload map[uint16]map[uint16]string
	if err := cbor.Unmarshal(sock.lastMessage().data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload[uint16(0x0400)][uint16(0x0402)]
}

func TestNavRouteAppendAndStaleRemove(t *testing.T) {
	mr := miniredis.RunT(t)
	var plan routePlan
	var methods []string
	var mu sync.Mutex
	startRouteServer(t, mr, func(env destinationEnvelope) []byte {
		mu.Lock()
		defer mu.Unlock()
		methods = append(methods, env.Method)
		switch env.Method {
		case "plan.get":
			return routeReply(plan)
		case "plan.append":
			var req routeAppendRequest
			if err := json.Unmarshal(env.Payload, &req); err != nil {
				t.Error(err)
			}
			if plan.ID == "" {
				plan.ID = "plan"
			}
			plan.Revision++
			plan.Stops = append(plan.Stops, routeStop{ID: fmt.Sprintf("stop-%d", plan.Revision), Lat: req.Stop.Lat, Lon: req.Stop.Lon, Label: req.Stop.Label})
			return routeReply(plan)
		case "plan.remove":
			var req routeRemoveRequest
			if err := json.Unmarshal(env.Payload, &req); err != nil {
				t.Error(err)
			}
			if req.ExpectedRevision != plan.Revision {
				return []byte(`{"ok":false,"error":"stale revision"}`)
			}
			plan.Stops = append(plan.Stops[:req.Index], plan.Stops[req.Index+1:]...)
			plan.Revision++
			return routeReply(plan)
		}
		t.Errorf("unexpected method %s", env.Method)
		return nil
	})
	s := newDestinationService(t, mr)
	sock := &mockUSOCK{}
	s.usock = sock
	s.handleNavCommand("route:add 52.5,13.4,Home")
	s.handleNavCommand("route:add 52.6,13.5,Work")
	if got := routeResponse(t, sock); got != "nav:route:count:2:0" {
		t.Errorf("response = %q", got)
	}
	mu.Lock()
	if len(plan.Stops) != 2 || plan.Stops[0].Label != "Home" || plan.Stops[1].Label != "Work" {
		t.Errorf("plan = %+v", plan)
	}
	mu.Unlock()
	s.handleNavCommand("route:remove 1")
	if got := routeResponse(t, sock); got != "nav:route:count:1:0" {
		t.Errorf("response = %q", got)
	}
	mu.Lock()
	if strings.Join(methods, ",") != "plan.append,plan.append,plan.get,plan.remove" {
		t.Errorf("methods = %v", methods)
	}
	mu.Unlock()
}

func TestNavRouteConflictAndUnavailable(t *testing.T) {
	mr := miniredis.RunT(t)
	startRouteServer(t, mr, func(env destinationEnvelope) []byte {
		switch env.Method {
		case "plan.get":
			return routeReply(routePlan{ID: "old", Revision: 2, Stops: []routeStop{{ID: "a"}, {ID: "b"}}})
		case "plan.remove", "plan.advance":
			return []byte(`{"ok":false,"error":"stale plan"}`)
		default:
			t.Errorf("unexpected %s", env.Method)
			return nil
		}
	})
	s := newDestinationService(t, mr)
	sock := &mockUSOCK{}
	s.usock = sock
	s.handleNavCommand("route:remove 1")
	if got := routeResponse(t, sock); !strings.HasPrefix(got, "nav:error:") {
		t.Errorf("conflict response = %q", got)
	}
	s.handleNavCommand("route:skip")
	if got := routeResponse(t, sock); !strings.HasPrefix(got, "nav:error:") {
		t.Errorf("progress response = %q", got)
	}
	s.destIPC = nil
	s.handleNavCommand("dest 52.5,13.4")
	if got := routeResponse(t, sock); !strings.HasPrefix(got, "nav:error:") {
		t.Errorf("unavailable response = %q", got)
	}
	if mr.Exists(KeyNavigation) {
		t.Error("navigation hash written on failure")
	}
}

func TestNavDestinationReplaceAndClear(t *testing.T) {
	mr := miniredis.RunT(t)
	envelopes := startRouteServer(t, mr, func(env destinationEnvelope) []byte { return routeReply(routePlan{}) })
	s := newDestinationService(t, mr)
	sock := &mockUSOCK{}
	s.usock = sock
	s.handleNavCommand("dest 52.5,13.4,Home")
	if got := routeResponse(t, sock); got != "nav:ok" {
		t.Errorf("response = %q", got)
	}
	env := <-envelopes
	if env.Method != "plan.replace" {
		t.Errorf("method = %s", env.Method)
	}
	var req routeReplaceRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Stops) != 1 || req.Stops[0].Label != "Home" {
		t.Errorf("request = %+v", req)
	}
	s.handleNavCommand("clear")
	if env := <-envelopes; env.Method != "plan.clear" {
		t.Errorf("method = %s", env.Method)
	}
	if mr.Exists(KeyNavigation) {
		t.Error("navigation hash written by BLE client")
	}
}

func TestNavRouteSkipGuardsCurrentStop(t *testing.T) {
	mr := miniredis.RunT(t)
	plan := routePlan{ID: "plan", Revision: 4, Stops: []routeStop{{ID: "first"}, {ID: "second"}}}
	envelopes := startRouteServer(t, mr, func(env destinationEnvelope) []byte {
		switch env.Method {
		case "plan.get":
			return routeReply(plan)
		case "plan.advance":
			plan.CurrentStep = 1
			plan.Revision++
			return routeReply(plan)
		default:
			t.Errorf("unexpected method %s", env.Method)
			return nil
		}
	})
	s := newDestinationService(t, mr)
	sock := &mockUSOCK{}
	s.usock = sock
	s.handleNavCommand("route:skip")
	if got := routeResponse(t, sock); got != "nav:route:count:2:1" {
		t.Errorf("response = %q", got)
	}
	for _, method := range []string{"plan.get", "plan.advance"} {
		env := <-envelopes
		if env.Method != method {
			t.Errorf("method = %q, want %q", env.Method, method)
		}
		if method != "plan.get" {
			var req routeProgressRequest
			if err := json.Unmarshal(env.Payload, &req); err != nil {
				t.Fatal(err)
			}
			if req.ExpectedPlanID != "plan" || req.ExpectedStopID != "first" {
				t.Errorf("progress = %+v", req)
			}
		}
	}
}

func TestSavedLocationAndNaviStartUseRouteOwner(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.HSet("settings", "dashboard.saved-locations.2.latitude", "52.5")
	mr.HSet("settings", "dashboard.saved-locations.2.longitude", "13.4")
	envelopes := startRouteServer(t, mr, func(env destinationEnvelope) []byte { return routeReply(routePlan{}) })
	s := newDestinationService(t, mr)
	// Saved-location lookup uses the string-codec client just like production.
	s.ipc = s.destIPC
	if err := s.navigateToSavedLocation("2"); err != nil {
		t.Fatal(err)
	}
	s.handleEventMessage(0, 0, "navi:start 52.6,13.5")
	for _, lat := range []float64{52.5, 52.6} {
		env := <-envelopes
		if env.Method != "plan.replace" {
			t.Errorf("method = %q", env.Method)
		}
		var req routeReplaceRequest
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			t.Fatal(err)
		}
		if len(req.Stops) != 1 || req.Stops[0].Lat != lat {
			t.Errorf("request = %+v", req)
		}
	}
	if mr.Exists(KeyNavigation) {
		t.Error("navigation hash written by BLE client")
	}
}

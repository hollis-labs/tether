package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/app/routingcap"
)

type routingRouteService struct{}

func (routingRouteService) RoutingCapabilities(_ context.Context, id string) (routingcap.RoutingCapabilitiesResponse, error) {
	if id == "missing" {
		return routingcap.RoutingCapabilitiesResponse{}, routingcap.ErrSessionNotFound
	}
	return routingcap.RoutingCapabilitiesResponse{SessionID: id, Delivery: "next-turn", KindsAvailable: []string{}, Runtimes: map[string]routingcap.RuntimeRoutingCapabilities{}}, nil
}

func TestRoutingCapabilitiesReachableThroughDaemon(t *testing.T) {
	h := (&Server{Routing: routingRouteService{}}).Handler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/routing/capabilities", 200},
		{"GET", "/routing/capabilities?session_id=session%2Fid", 200},
		{"GET", "/routing/capabilities?session_id=missing", 404},
		{"POST", "/routing/capabilities", 405},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s %s = %d: %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.status == 200 {
			var got routingcap.RoutingCapabilitiesResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Delivery != "next-turn" {
				t.Fatalf("response = %+v", got)
			}
			if tc.path != "/routing/capabilities" && got.SessionID != "session/id" {
				t.Fatalf("session = %q", got.SessionID)
			}
		}
	}
	w := httptest.NewRecorder()
	(&Server{}).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/routing/capabilities", nil))
	if w.Code != 404 {
		t.Fatalf("disabled = %d", w.Code)
	}
}

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/identity"
)

// POST /events with a summary and scope.cell stores both, and GET /events
// returns them; a multi-line summary is refused (CW-20261114-0019).
func TestIngestSummaryAndCellRoundTrip(t *testing.T) {
	hub := newConfHub(t, identity.Off, nil, eventingest.Config{}, true)
	post := func(body string) []ingest.Result {
		t.Helper()
		resp, err := http.Post(hub.srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out struct {
			Results []ingest.Result `json:"results"`
		}
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Results) != 1 {
			t.Fatalf("POST /events: %d %+v", resp.StatusCode, out)
		}
		return out.Results
	}
	event := func(id, summary string) string {
		b, _ := json.Marshal(map[string]any{
			"schema_version": "1", "id": id, "kind": "alert.raised", "time": "2026-11-14T12:00:00Z",
			"app": "cerberus", "sequence": 1, "generation": 1, "subject": "msg://service/cerberus/dev",
			"source":  map[string]any{"channel": "test", "confidence": "exact"},
			"summary": summary, "scope": map[string]any{"cell": "photon"},
		})
		return string(b)
	}

	ok := "evt_00000000000000000000000000001930"
	if res := post(event(ok, "disk full on worker-1")); res[0].Status != ingest.StatusAccepted {
		t.Fatalf("accept: %+v", res)
	}
	bad := "evt_00000000000000000000000000001931"
	if res := post(event(bad, "disk full\non worker-1")); res[0].Status != ingest.StatusRejected || res[0].Code != ingest.CodeSchemaInvalid {
		t.Fatalf("multi-line: %+v", res)
	}

	s := &Server{EventsStore: hub.db}
	w := httptest.NewRecorder()
	s.handleEvents(w, httptest.NewRequest(http.MethodGet, "/events", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /events: %d %s", w.Code, w.Body)
	}
	var list EventListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Events) != 1 || list.Events[0].Summary != "disk full on worker-1" || list.Events[0].Cell != "photon" {
		t.Fatalf("GET /events = %s", w.Body)
	}
}

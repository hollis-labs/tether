package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/environmentstream"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEventIdentityHealthReadinessCountsOnly(t *testing.T) {
	for _, tc := range []struct {
		ready     bool
		failed    int64
		available bool
		status    string
	}{{false, 0, true, "starting"}, {false, 2, true, "degraded"}, {false, 0, false, "degraded"}, {true, 2, true, "ok"}} {
		s := &Server{EventIdentity: func() *environmentstream.IdentityHealth {
			h := &environmentstream.IdentityHealth{Ready: tc.ready}
			if tc.available {
				h.Progress = &store.EventIdentityProgress{EnvironmentID: "env", Cutover: 5, Remaining: 1, Failed: tc.failed}
			}
			return h
		}}
		rec := httptest.NewRecorder()
		s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var h Health
		if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || h.Status != tc.status || h.EventIdentity == nil || h.EventIdentity.Ready != tc.ready || (h.EventIdentity.Progress != nil) != tc.available || strings.Contains(rec.Body.String(), "error_text") {
			t.Fatal(rec.Code, rec.Body.String())
		}
		if tc.available && h.EventIdentity.Progress.Failed != tc.failed {
			t.Fatal("failure count lost")
		}
	}
}

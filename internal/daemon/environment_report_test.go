package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/environment/report"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEnvironmentReportMountedVerifiedReadAuthority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	read, err := ids.Mint(context.Background(), identity.Principal{ID: "reader", Kind: "service", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	write, err := ids.Mint(context.Background(), identity.Principal{ID: "writer", Kind: "service", Scopes: []string{"write"}})
	if err != nil {
		t.Fatal(err)
	}
	operator, err := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	api := &report.API{
		WorkRoot: filepath.Join(t.TempDir(), "unavailable-workspace"),
		Sampler:  report.NewSampler(0),
		Detectors: &report.Detectors{
			LookPath: func(string) (string, error) { return "", errors.New("not installed") },
			ExecCommand: func(context.Context, string, ...string) ([]byte, error) {
				probes++
				return nil, errors.New("unknown")
			},
		},
	}
	s := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: ids, EnvironmentReport: api}
	t.Cleanup(s.CloseIdentityAudit)
	h := s.Handler()
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"anonymous", "", 401},
		{"invalid", "invalid", 401},
		{"write only", write, 403},
		{"read", read, 200},
		{"operator universal", operator, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := probes
			r := httptest.NewRequest(http.MethodGet, "/v1/environment/report", nil)
			r.Header.Set("X-Forwarded-User-Id", identity.OperatorID)
			r.Header.Set("X-Scopes", "read,*")
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
			if tc.want != 200 {
				if probes != before || w.Body.Len() != 0 {
					t.Fatal("unauthorized caller reached report detectors or details")
				}
				return
			}
			var got report.Report
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.Providers["codex"].LoggedIn != "unknown" {
				t.Fatal("mounted report did not preserve unknown provider state", err)
			}
		})
	}
	// Existing observe-mode behavior outside the protected report is preserved.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health", nil))
	if w.Code != 200 {
		t.Fatal("report registration changed public health")
	}
	s.Config.IdentityMode = identity.Off
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/environment/report", nil)
	r.Header.Set("Authorization", "Bearer "+operator)
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("identity-off accepted an unverified bearer for report")
	}
}

package report

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestAPI_Auth(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := &Detectors{
		LookPath: func(string) (string, error) { return "", errors.New("not installed") },
		ExecCommand: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("undetectable")
		},
	}
	s := NewSampler(0)

	api := &API{
		Detectors: d,
		Sampler:   s,
		WorkRoot:  t.TempDir(),
	}

	tests := []struct {
		name       string
		scopes     []string
		hasContext bool
		wantStatus int
	}{
		{"anonymous", nil, false, http.StatusUnauthorized},
		{"invalid_scope", []string{"write"}, true, http.StatusForbidden},
		{"read_scope", []string{"read"}, true, http.StatusOK},
		{"operator_universal", []string{"*"}, true, http.StatusOK},
		{"mixed_scopes", []string{"write", "read"}, true, http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/environment/report", nil)
			if tt.hasContext {
				p := identity.Principal{Scopes: tt.scopes}
				ctx := identity.WithPrincipal(req.Context(), p)
				req = req.WithContext(ctx)
			}

			w := httptest.NewRecorder()
			api.ServeHTTP(w, req)

			if tt.wantStatus == http.StatusOK {
				var got Report
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.BubblewrapUsable != nil || got.Hosting.SystemdUserSession != "unknown" || got.Hosting.LingerEnabled != "unknown" || got.Resources.Status != "unknown" || got.Resources.MemoryAvailable != nil {
					t.Fatalf("unknown detector state lost on wire: %+v", got)
				}
				if w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("report could be cached")
				}
			}
			if w.Code != tt.wantStatus {
				t.Errorf("got status %v, want %v", w.Code, tt.wantStatus)
			}
		})
	}
}

package report

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestAPI_Auth(t *testing.T) {
	d := DefaultDetectors() // using empty injected ones where needed
	s := NewSampler(0)

	api := &API{
		Detectors: d,
		Sampler:   s,
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

			if w.Code != tt.wantStatus {
				t.Errorf("got status %v, want %v", w.Code, tt.wantStatus)
			}
		})
	}
}

package identity_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

type verifierFunc func(context.Context, string) (identity.Principal, error)

func (f verifierFunc) Verify(ctx context.Context, token string) (identity.Principal, error) {
	return f(ctx, token)
}

func TestIdentityMiddlewareModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	good, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []identity.Mode{identity.Off, identity.Observe, identity.Enforce} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + good} {
			t.Run(string(mode)+"/"+strings.Split(auth, " ")[0], func(t *testing.T) {
				var observation *identity.Observation
				var called, carried bool
				verify := verifierFunc(func(_ context.Context, token string) (identity.Principal, error) {
					if token != good {
						return identity.Principal{}, identity.ErrInvalidToken
					}
					return identity.Principal{ID: "verified", Kind: "session", SessionID: "s"}, nil
				})
				handler := identity.Middleware(mode, verify, func(_ context.Context, o identity.Observation) error { observation = &o; return nil }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					called = true
					p, ok := identity.FromContext(r.Context())
					carried = ok && p.ID == "verified"
					w.WriteHeader(http.StatusNoContent)
				}))
				req := httptest.NewRequest(http.MethodPost, "/sessions/s?token=do-not-record", nil)
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				req.Header.Set("X-Forwarded-User-Id", "forged")
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, req)
				valid := auth == "Bearer "+good
				wantCalled := mode != identity.Enforce || valid
				if called != wantCalled || carried != (mode != identity.Off && valid) {
					t.Fatalf("called=%v principal=%v", called, carried)
				}
				if mode == identity.Off || auth == "" {
					if observation != nil {
						t.Fatal("off/anonymous mode recorded identity")
					}
					return
				}
				if observation == nil || observation.Route != "/sessions" {
					t.Fatalf("observation=%+v", observation)
				}
				if valid && (observation.PrincipalID != "verified" || observation.SessionID != "s") {
					t.Fatal("verified attribution missing")
				}
				if !wantCalled && out.Code != http.StatusUnauthorized {
					t.Fatalf("status=%d", out.Code)
				}
			})
		}
	}
}

func TestIdentityObserveSurvivesUnavailableAndHealthStaysOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []identity.Mode{identity.Observe, identity.Enforce} {
		handler := identity.Middleware(mode, verifierFunc(func(context.Context, string) (identity.Principal, error) {
			return identity.Principal{}, errors.New("storage failure")
		}), func(context.Context, identity.Observation) error { return errors.New("audit failure") }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		for _, path := range []string{"/health", "/sessions"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer unavailable")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			want := http.StatusNoContent
			if path != "/health" && mode == identity.Enforce {
				want = http.StatusServiceUnavailable
			}
			if out.Code != want {
				t.Fatalf("%s %s: status=%d want=%d", mode, path, out.Code, want)
			}
		}
	}
}

func TestIdentityValidateBind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, addr := range []string{"unix:/tmp/test.sock", "tcp:127.0.0.1:7180", "tcp:[::1]:7180", "tcp:localhost:7180"} {
		if err := identity.ValidateBind(addr, identity.Observe); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []identity.Mode{identity.Off, identity.Observe} {
		for _, addr := range []string{"tcp:0.0.0.0:7180", "tcp:[::]:7180", "tcp:example.com:7180"} {
			if err := identity.ValidateBind(addr, mode); err == nil {
				t.Fatalf("unrestricted bind accepted: %s %s", mode, addr)
			}
		}
	}
	if err := identity.ValidateBind("tcp:0.0.0.0:7180", identity.Enforce); err != nil {
		t.Fatal(err)
	}
	if err := identity.ValidateBind("unix:/tmp/test.sock", "typo"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

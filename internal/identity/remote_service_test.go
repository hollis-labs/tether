package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stubJWT struct {
	p   Principal
	err error
}

func (s stubJWT) Verify(_ context.Context, _ string) (Principal, error) { return s.p, s.err }

const testJWT = "aaaa.bbbb.cccc"

func goodService() Principal {
	exp := time.Now().Add(time.Hour)
	return Principal{ID: "msg://service/cerberus/dev", Kind: "service", TokenID: "jti-live-1", Audience: "tether",
		Scopes: []string{"tether.events"}, ExpiresAt: &exp, Verified: true}
}

func remoteWith(svc ServiceJWT, device Verifier) (http.Handler, *int, *Principal) {
	reached := 0
	var got Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		got, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	return RemoteMiddleware(device, nil, next, WithServiceJWT(svc)), &reached, &got
}

func do(h http.Handler, method, target, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader("{}"))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func live(jtis ...string) func(string) bool {
	return func(j string) bool {
		for _, x := range jtis {
			if x == j {
				return true
			}
		}
		return false
	}
}

func TestServiceJWTAcceptedOnExactlyPostEvents(t *testing.T) {
	h, reached, got := remoteWith(ServiceJWT{Verify: stubJWT{p: goodService()}, Known: live("jti-live-1")}, nil)
	if w := do(h, "POST", "/events", testJWT); w.Code != 204 || *reached != 1 || got.ID != "msg://service/cerberus/dev" || !got.Verified {
		t.Fatalf("accepted request: %d reached=%d principal=%+v", w.Code, *reached, got)
	}
}

func TestServiceJWTMarkerIsSetOnlyOnTheAcceptedPath(t *testing.T) {
	var marked bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		marked = ServiceEventsRequest(r.Context())
		w.WriteHeader(204)
	})
	h := RemoteMiddleware(nil, nil, next, WithServiceJWT(ServiceJWT{Verify: stubJWT{p: goodService()}, Known: live("jti-live-1")}))
	if w := do(h, "POST", "/events", testJWT); w.Code != 204 || !marked {
		t.Fatalf("%d marked=%v", w.Code, marked)
	}
}

func TestEveryOtherRouteAndVariantStaysRefused(t *testing.T) {
	h, reached, _ := remoteWith(ServiceJWT{Verify: stubJWT{p: goodService()}, Known: live("jti-live-1")}, deviceStub{})
	for _, c := range []struct{ method, target string }{
		{"GET", "/events"}, {"HEAD", "/events"}, {"PUT", "/events"}, {"DELETE", "/events"}, {"PATCH", "/events"},
		{"POST", "/events/"}, {"POST", "/events?x=1"}, {"POST", "/events?"}, {"POST", "//events"}, {"POST", "/Events"},
		{"POST", "/events/x"}, {"POST", "/events/stream"}, {"POST", "/proxy/events"}, {"POST", "/%65vents"}, {"POST", "/events%2F"},
		{"POST", "/events;x"}, {"POST", "/messages"}, {"GET", "/sessions"}, {"POST", "/sessions"}, {"GET", "/environment/events"},
		{"POST", "/auth/token"}, {"POST", "/auth/pair"}, {"POST", "/auth/renew"}, {"GET", "/auth/devices"}, {"POST", "/auth/service-tokens"},
		{"POST", "/worker/v1/connect"}, {"POST", "/mcp"}, {"GET", "/"},
	} {
		if w := do(h, c.method, c.target, testJWT); w.Code != 401 {
			t.Errorf("%s %s with a service JWT: %d, want 401", c.method, c.target, w.Code)
		}
	}
	if *reached != 0 {
		t.Fatalf("%d refused requests reached the handler", *reached)
	}
}

func TestServiceJWTGate(t *testing.T) {
	mod := func(f func(*Principal)) Principal { p := goodService(); f(&p); return p }
	for _, c := range []struct {
		name  string
		v     stubJWT
		known []string
		want  int
	}{
		{"ok", stubJWT{p: goodService()}, []string{"jti-live-1"}, 204},
		{"invalid signature or expired or revoked", stubJWT{err: ErrInvalidToken}, []string{"jti-live-1"}, 401},
		{"verifier unavailable", stubJWT{err: errors.New("keys unreadable")}, []string{"jti-live-1"}, 503},
		{"agent kind", stubJWT{p: mod(func(p *Principal) { p.Kind = "agent" })}, []string{"jti-live-1"}, 401},
		{"operator-minted narrow token", stubJWT{p: mod(func(p *Principal) { p.Kind = "operator"; p.ID = OperatorID })}, []string{"jti-live-1"}, 401},
		{"device kind", stubJWT{p: mod(func(p *Principal) { p.Kind = "device" })}, []string{"jti-live-1"}, 401},
		{"no jti", stubJWT{p: mod(func(p *Principal) { p.TokenID = "" })}, []string{""}, 401},
		{"not a recorded service token", stubJWT{p: goodService()}, []string{"another-jti"}, 401},
		{"wrong audience", stubJWT{p: mod(func(p *Principal) { p.Audience = "tachyon" })}, []string{"jti-live-1"}, 401},
		{"bad principal id", stubJWT{p: mod(func(p *Principal) { p.ID = "msg://user/someone" })}, []string{"jti-live-1"}, 401},
		{"principal id with a path", stubJWT{p: mod(func(p *Principal) { p.ID = "msg://service/a/b/c" })}, []string{"jti-live-1"}, 401},
		{"no exp", stubJWT{p: mod(func(p *Principal) { p.ExpiresAt = nil })}, []string{"jti-live-1"}, 401},
		{"no events scope", stubJWT{p: mod(func(p *Principal) { p.Scopes = []string{"tether.read"} })}, []string{"jti-live-1"}, 403},
		{"no scopes", stubJWT{p: mod(func(p *Principal) { p.Scopes = nil })}, []string{"jti-live-1"}, 403},
	} {
		h, reached, _ := remoteWith(ServiceJWT{Verify: c.v, Known: live(c.known...)}, nil)
		w := do(h, "POST", "/events", testJWT)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d (%s)", c.name, w.Code, c.want, w.Body)
		}
		if (c.want == 204) != (*reached == 1) {
			t.Errorf("%s: reached=%d", c.name, *reached)
		}
		if c.want == 503 && w.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", c.name)
		}
	}
}

type deviceStub struct{}

func (deviceStub) Verify(_ context.Context, tok string) (Principal, error) {
	if strings.HasPrefix(tok, "tth_") {
		return Principal{ID: "msg://device/d1", Kind: "device", Scopes: []string{"worker"}, Verified: true}, nil
	}
	return Principal{}, ErrInvalidToken
}

func TestPathIsAbsentWithoutTheOptionAndDevicesAreUnchanged(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	// Not configured: a JWT is simply refused, like any non-device bearer.
	h := RemoteMiddleware(deviceStub{}, nil, next)
	if w := do(h, "POST", "/events", testJWT); w.Code != 401 {
		t.Fatalf("unconfigured: %d", w.Code)
	}
	// An option with missing parts is not enabled either (fail closed).
	h = RemoteMiddleware(deviceStub{}, nil, next, WithServiceJWT(ServiceJWT{Verify: stubJWT{p: goodService()}}))
	if w := do(h, "POST", "/events", testJWT); w.Code != 401 {
		t.Fatalf("option without Known: %d", w.Code)
	}
}

func TestDeviceAndJWTCredentialsNeverCross(t *testing.T) {
	// A verifier that would accept anything for the JWT path must never see a tth_ credential,
	// and the device path must never see a JWT.
	seen := ""
	spy := verifierFunc(func(_ context.Context, tok string) (Principal, error) { seen = tok; return goodService(), nil })
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := RemoteMiddleware(deviceStub{}, nil, next, WithServiceJWT(ServiceJWT{Verify: spy, Known: live("jti-live-1")}))
	if w := do(h, "POST", "/events", "tth_"+strings.Repeat("A", 43)); seen != "" {
		t.Fatalf("a device credential reached the JWT verifier (%d)", w.Code)
	}
	if w := do(h, "POST", "/events", testJWT); w.Code != 204 || seen != testJWT {
		t.Fatalf("jwt: %d seen=%q", w.Code, seen)
	}
	// A JWT on a device route is refused by the device path without the JWT verifier.
	seen = ""
	if w := do(h, "GET", "/sessions", testJWT); w.Code != 401 || seen != "" {
		t.Fatalf("jwt on a device route: %d seen=%q", w.Code, seen)
	}
}

type verifierFunc func(context.Context, string) (Principal, error)

func (f verifierFunc) Verify(ctx context.Context, tok string) (Principal, error) { return f(ctx, tok) }

func TestFailedAttemptBudgetBoundsUnauthenticatedWork(t *testing.T) {
	now := time.Now()
	lim := NewAttemptLimiter(1, 3)
	lim.now = func() time.Time { return now }
	calls := 0
	v := verifierFunc(func(context.Context, string) (Principal, error) { calls++; return Principal{}, ErrInvalidToken })
	h, _, _ := remoteWith(ServiceJWT{Verify: v, Known: live(), Failures: lim}, nil)
	for i := 0; i < 3; i++ {
		if w := do(h, "POST", "/events", testJWT); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	if w := do(h, "POST", "/events", testJWT); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("over budget: %d", w.Code)
	}
	if calls != 3 {
		t.Fatalf("verifier ran %d times; the flood must not cost a verification once the budget is spent", calls)
	}
	now = now.Add(2 * time.Second)
	if w := do(h, "POST", "/events", testJWT); w.Code != 401 || calls != 4 {
		t.Fatalf("after refill: %d calls=%d", w.Code, calls)
	}
}

func TestSuccessfulRequestsAreNotChargedToTheFailureBudget(t *testing.T) {
	lim := NewAttemptLimiter(0.001, 1)
	h, _, _ := remoteWith(ServiceJWT{Verify: stubJWT{p: goodService()}, Known: live("jti-live-1"), Failures: lim}, nil)
	for i := 0; i < 20; i++ {
		if w := do(h, "POST", "/events", testJWT); w.Code != 204 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
}

func TestAuditLineNeverContainsTheToken(t *testing.T) {
	var rec []Observation
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := RemoteMiddleware(nil, func(_ context.Context, o Observation) error { rec = append(rec, o); return nil }, next,
		WithServiceJWT(ServiceJWT{Verify: stubJWT{p: goodService()}, Known: live("jti-live-1")}))
	do(h, "POST", "/events", testJWT)
	if len(rec) != 1 || rec[0].PrincipalID != "msg://service/cerberus/dev" || rec[0].Event != "service_jwt_accepted" || !rec[0].Verified {
		t.Fatalf("%+v", rec)
	}
	if strings.Contains(rec[0].Event+rec[0].Route+rec[0].PrincipalID, testJWT) {
		t.Fatal("token in the audit row")
	}
}

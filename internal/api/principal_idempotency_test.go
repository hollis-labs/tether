package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestRemoteIdempotencyUsesActualPrincipal(t *testing.T) {
	for _, path := range []string{"/sessions", "/logical-agents/agent/resume"} {
		svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "s"}, resumeRes: LaunchResult{SessionID: "s"}}
		h := newTestHandler(svc)
		for _, id := range []string{"msg://device/a", "msg://device/a", "msg://device/b"} {
			ctx := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{Verified: true, ID: id, Kind: "device", Display: "same label", Scopes: []string{"operate"}})
			body := `{"launch":"demo","idempotency_key":"same"}`
			if strings.Contains(path, "resume") {
				body = `{"idempotency_key":"same"}`
			}
			r := httptest.NewRequest("POST", path+"?as=msg://device/forged", strings.NewReader(body)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 201 {
				t.Fatalf("request refused %d", w.Code)
			}
		}
		var keys, principals []string
		if path == "/sessions" {
			for _, req := range svc.createInputs {
				keys = append(keys, req.IdempotencyKey)
				principals = append(principals, req.IdempotencyPrincipal)
			}
		} else {
			for _, req := range svc.resumeOpts {
				keys = append(keys, req.IdempotencyKey)
				principals = append(principals, req.IdempotencyPrincipal)
			}
		}
		want := []string{"msg://device/a", "msg://device/a", "msg://device/b"}
		for i := range want {
			if len(keys) != 3 || keys[i] != "same" || principals[i] != want[i] {
				t.Fatalf("key scope = %q/%q, want same/%q", keys, principals, want)
			}
		}
	}
	for _, key := range []string{"", "same"} {
		if _, err := idempotencyPrincipal(identity.WithRemoteContext(context.Background())); err == nil {
			t.Fatalf("remote key %q silently lost authentication", key)
		}
	}
}

// A local caller is scoped to its verified principal; an unverified one is
// left empty and shares the service's anonymous space.
func TestLocalIdempotencyPrincipal(t *testing.T) {
	if p, err := idempotencyPrincipal(context.Background()); err != nil || p != "" {
		t.Fatalf("unverified local principal = %q, %v", p, err)
	}
	ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://session/local/s1", Kind: "session"})
	if p, err := idempotencyPrincipal(ctx); err != nil || p != "msg://session/local/s1" {
		t.Fatalf("verified local principal = %q, %v", p, err)
	}
	for _, path := range []string{"/sessions", "/logical-agents/agent/resume"} {
		svc := &fakeLaunchService{createRes: LaunchResult{SessionID: "s"}, resumeRes: LaunchResult{SessionID: "s"}}
		h := newTestHandler(svc)
		for _, id := range []string{"msg://session/local/s1", "msg://session/local/s2"} {
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: id, Kind: "session"})
			body := `{"launch":"demo","idempotency_key":"same","idempotency_principal":"msg://user/local/operator"}`
			if strings.Contains(path, "resume") {
				body = `{"idempotency_key":"same","idempotency_principal":"msg://user/local/operator"}`
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)).WithContext(ctx))
			if w.Code != 201 {
				t.Fatalf("request refused %d", w.Code)
			}
		}
		var got []string
		for _, req := range svc.createInputs {
			got = append(got, req.IdempotencyPrincipal)
		}
		for _, req := range svc.resumeOpts {
			got = append(got, req.IdempotencyPrincipal)
		}
		if len(got) != 2 || got[0] != "msg://session/local/s1" || got[1] != "msg://session/local/s2" {
			t.Fatalf("%s principals = %q; a body field must not choose the scope", path, got)
		}
	}
}

func TestRemoteReplyIdempotency(t *testing.T) {
	svc := &fakeReplies{}
	s := &Server{RoutingReplies: svc}
	for _, id := range []string{"msg://device/a", "msg://device/b"} {
		ctx := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{Verified: true, ID: id, Kind: "device", Scopes: []string{"operate"}})
		r := httptest.NewRequest("POST", "/messages/parent/reply?as=msg://device/forged", nil).WithContext(ctx)
		r.Header.Set("Idempotency-Key", "same")
		if _, ok := s.submitReply(httptest.NewRecorder(), r, "parent", "body", false, "", nil); !ok {
			t.Fatal("reply refused")
		}
	}
	// The store keys a reply by (parent, actor, key); the actor is the verified caller.
	if len(svc.requests) != 2 || svc.requests[0].IdempotencyKey != "same" || svc.requests[0].Caller.ID != "msg://device/a" || svc.requests[1].Caller.ID != "msg://device/b" {
		t.Fatal("reply bound to supplied identity")
	}
}

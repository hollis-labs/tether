package api

import (
	"context"
	"net/http"
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
			ctx := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{ID: id, Kind: "device", Display: "same label", Scopes: []string{"operate"}})
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
		var keys []string
		if path == "/sessions" {
			for _, req := range svc.createInputs {
				keys = append(keys, req.IdempotencyKey)
			}
		} else {
			for _, req := range svc.resumeOpts {
				keys = append(keys, req.IdempotencyKey)
			}
		}
		if len(keys) != 3 || keys[0] != keys[1] || keys[0] == keys[2] || keys[0] == "same" {
			t.Fatal("authenticated device namespaces collide or replay changed")
		}
	}
	if key, err := principalIdempotencyKey(context.Background(), "same"); err != nil || key != "same" {
		t.Fatal("local namespace changed")
	}
	for _, key := range []string{"", "same"} {
		if _, err := principalIdempotencyKey(identity.WithRemoteContext(context.Background()), key); err == nil {
			t.Fatal("remote key silently lost authentication")
		}
	}
}

func TestRemoteReplyAndTeamIdempotency(t *testing.T) {
	svc := &fakeReplies{}
	s := &Server{RoutingReplies: svc}
	for _, id := range []string{"msg://device/a", "msg://device/b"} {
		ctx := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{ID: id, Kind: "device", Scopes: []string{"operate"}})
		r := httptest.NewRequest("POST", "/messages/parent/reply?as=msg://device/forged", nil).WithContext(ctx)
		r.Header.Set("Idempotency-Key", "same")
		if _, ok := s.submitReply(httptest.NewRecorder(), r, "parent", "body", false, ""); !ok {
			t.Fatal("reply refused")
		}
	}
	if len(svc.requests) != 2 || svc.requests[0].IdempotencyKey == svc.requests[1].IdempotencyKey || svc.requests[0].Caller.ID != "msg://device/a" {
		t.Fatal("reply bound to supplied identity")
	}
	var keys []string
	for _, id := range []string{"msg://device/a", "msg://device/b"} {
		ctx := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{ID: id, Kind: "device", Scopes: []string{"operate"}})
		r := httptest.NewRequest("POST", "/teams/form", nil).WithContext(ctx)
		r.Header.Set("Idempotency-Key", "same")
		RemoteScopeMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { keys = append(keys, r.Header.Get("Idempotency-Key")) })).ServeHTTP(httptest.NewRecorder(), r)
	}
	if len(keys) != 2 || keys[0] == keys[1] {
		t.Fatal("team key namespaces collide")
	}
}

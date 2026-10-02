package mcpadapter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/identity"
)

type contextRoundTripFunc func(*http.Request) (*http.Response, error)

func (f contextRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sessionEgressContext(id string) context.Context {
	p := identity.Principal{ID: "principal:" + id, Kind: "session", SessionID: id}
	ctx := identity.WithPrincipal(context.Background(), p)
	return withForwardedToolCall(callcontext.WithSnapshot(ctx, callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: p.ID, PrincipalKind: p.Kind, SessionID: id, AgentURN: "msg://agent/" + id, WorkstreamID: "w:" + id}))
}

func TestForwardedContextAdmissionAndIsolation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origin, err := url.Parse("https://upstream.example/mcp")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := make(map[string]http.Header)
	transport := &forwardedContextTransport{origin: origin, serviceToken: "upstream-service", base: contextRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		seen[req.URL.Query().Get("case")] = req.Header.Clone()
		mu.Unlock()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	})}
	verified := sessionEgressContext("one")
	snapshotOnly := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: "forged", PrincipalKind: "session", SessionID: "one"})
	mismatched := identity.WithPrincipal(verified, identity.Principal{ID: "other", Kind: "session", SessionID: "one"})
	operator := identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator"})
	operator = callcontext.WithSnapshot(operator, callcontext.Snapshot{Source: "daemon", PrincipalID: identity.OperatorID, PrincipalKind: "operator"})
	cases := []struct {
		name, method string
		ctx          context.Context
		session      string
	}{
		{"verified", http.MethodPost, verified, "one"},
		{"anonymous", http.MethodPost, context.Background(), ""},
		{"initialize", http.MethodPost, callcontext.WithSnapshot(identity.WithPrincipal(context.Background(), identity.Principal{ID: "principal:one", Kind: "session", SessionID: "one"}), callcontext.Snapshot{Verified: true, Source: "daemon", PrincipalID: "principal:one", PrincipalKind: "session", SessionID: "one"}), ""},
		{"snapshot-only", http.MethodPost, snapshotOnly, ""},
		{"mismatch", http.MethodPost, mismatched, ""},
		{"operator", http.MethodPost, operator, ""},
		{"background-get", http.MethodGet, verified, ""},
	}
	for _, tc := range cases {
		req, err := http.NewRequestWithContext(tc.ctx, tc.method, origin.String()+"?case="+tc.name, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header["x-forwarded-user-id"] = []string{"operator"}
		req.Header.Set("X-Tether-Session-Id", "forged")
		req.Header.Set("X-Forwarded-Host", "forged")
		req.Header.Set("Authorization", "Bearer session-secret")
		req.Header.Set("Traceparent", "independent")
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		got := seen[tc.name]
		if got.Get("Authorization") != "Bearer upstream-service" {
			t.Fatal("service authentication missing")
		}
		wantActor := ""
		if tc.session != "" {
			wantActor = "session:" + tc.session
		}
		if got.Get("X-Forwarded-User-Id") != wantActor || got.Get("X-Tether-Session-Id") != tc.session {
			t.Fatalf("%s: wrong attribution", tc.name)
		}
		if got.Get("X-Forwarded-Host") != "" || got.Get("Traceparent") != "independent" {
			t.Fatal("header sanitization failed")
		}
		if req.Header.Get("Authorization") != "Bearer session-secret" || req.Header.Get("X-Tether-Session-Id") != "forged" {
			t.Fatal("caller request mutated")
		}
	}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("session-%d", i)
			req, err := http.NewRequestWithContext(sessionEgressContext(id), http.MethodPost, origin.String()+"?case="+id, nil)
			if err != nil {
				t.Error(err)
				return
			}
			resp, err := transport.RoundTrip(req)
			if err != nil {
				t.Error(err)
				return
			}
			_ = resp.Body.Close()
		}()
	}
	wg.Wait()
	for i := range 32 {
		id := fmt.Sprintf("session-%d", i)
		if seen[id].Get("X-Forwarded-User-Id") != "session:"+id || seen[id].Get("X-Tether-Agent-Urn") != "msg://agent/"+id {
			t.Fatal("concurrent callers crossed")
		}
	}
	req, err := http.NewRequest(http.MethodPost, "https://other.example/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("cross-origin credential forwarding accepted")
	}
}

func TestServiceHTTPFactoryReconnectAndFailClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	tokenPath := filepath.Join(dir, "service.token")
	if err := os.WriteFile(tokenPath, []byte("upstream-service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	headers := make(chan http.Header, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	factory, err := serviceHTTPClientFactory(server.URL, tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	fixed := map[string]string{"Authorization": "Bearer wrong", "X-Forwarded-User-Id": "operator", "X-Tether-Session-Id": "forged"}
	for _, ctx := range []context.Context{sessionEgressContext("A"), context.Background(), sessionEgressContext("B")} {
		// New builder invocation models reconnect. No previous caller may survive it.
		client := factory(fixed, 1)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		got := <-headers
		snapshot, _ := callcontext.FromContext(ctx)
		want := ""
		if snapshot.Verified {
			want = "session:" + snapshot.SessionID
		}
		if got.Get("Authorization") != "Bearer upstream-service" || got.Get("X-Forwarded-User-Id") != want || got.Get("X-Tether-Session-Id") != snapshot.SessionID {
			t.Fatal("reconnect leaked caller context or service auth missing")
		}
	}
	if fixed["X-Forwarded-User-Id"] != "operator" {
		t.Fatal("configured headers mutated")
	}
	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceHTTPClientFactory(server.URL, tokenPath); err == nil {
		t.Fatal("unsafe token file admitted")
	}
	if _, err := serviceHTTPClientFactory("https://user:secret@upstream.example", tokenPath); err == nil {
		t.Fatal("userinfo admitted")
	}
}

package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

// tether_session_create passes continuity to POST /sessions, leaves the default
// unchanged and rejects other values before any request is made.
func TestSessionCreateContinuity(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"s1"}`))
	}))
	defer srv.Close()
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")), "tok", []string{ScopeSessionWrite})
	s := gomcp.NewServer("test", "0.0.1")
	a.registerSessionTools(s)
	c := connectInMemory(t, s)
	call := func(args map[string]any) *mcpsdk.CallToolResult {
		t.Helper()
		res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_session_create", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	for _, tc := range []struct{ in, want string }{{"relaunch", "relaunch"}, {"fresh", "fresh"}, {"", ""}} {
		args := map[string]any{"launch_id": "l1"}
		if tc.in != "" {
			args["continuity"] = tc.in
		}
		if res := call(args); res.IsError {
			t.Fatalf("continuity %q: %s", tc.in, textOf(res))
		}
		got, _ := bodies[len(bodies)-1]["continuity"].(string)
		if got != tc.want {
			t.Fatalf("continuity %q sent %q, want %q", tc.in, got, tc.want)
		}
	}

	n := len(bodies)
	res := call(map[string]any{"launch_id": "l1", "continuity": "resume"})
	if !res.IsError || parseToolJSON(t, res)["code"] != "invalid_request" {
		t.Fatalf("invalid continuity not rejected: %s", textOf(res))
	}
	if len(bodies) != n {
		t.Fatal("invalid continuity reached the daemon")
	}
}

// tether_session_launch continuity=relaunch refuses to start a session that was
// created fresh, and starts a continuation.
func TestSessionLaunchAssertsContinuity(t *testing.T) {
	var mu sync.Mutex
	launched := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/fresh":
			_, _ = w.Write([]byte(`{"id":"fresh","intent":"fresh"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/cont":
			_, _ = w.Write([]byte(`{"id":"cont","intent":"resume","parent_session_id":"p1"}`))
		case r.Method == http.MethodPost:
			launched++
			_, _ = w.Write([]byte(`{"id":"cont"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	a := NewWithDaemon(&app.Service{}, client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://")), "tok", []string{ScopeSessionWrite})
	s := gomcp.NewServer("test", "0.0.1")
	a.registerSessionTools(s)
	c := connectInMemory(t, s)
	call := func(args map[string]any) *mcpsdk.CallToolResult {
		t.Helper()
		res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_session_launch", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(map[string]any{"session_id": "fresh", "continuity": "relaunch"}); !res.IsError || parseToolJSON(t, res)["code"] != "invalid_request" || launched != 0 {
		t.Fatalf("fresh session launched as a continuation: %s launched=%d", textOf(res), launched)
	}
	if res := call(map[string]any{"session_id": "cont", "continuity": "relaunch"}); res.IsError || launched != 1 {
		t.Fatalf("continuation not launched: %s launched=%d", textOf(res), launched)
	}
	if res := call(map[string]any{"session_id": "fresh"}); res.IsError || launched != 2 {
		t.Fatalf("plain launch changed: %s launched=%d", textOf(res), launched)
	}
}

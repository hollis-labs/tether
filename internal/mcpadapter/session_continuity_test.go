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

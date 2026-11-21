package mcptransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/runtoken"
	"github.com/hollis-labs/tether/internal/store"
)

func runFixture(t *testing.T) (*transportFixture, string, ed25519.PrivateKey, runtoken.Claims) {
	t.Helper()
	return runFixtureWithServers(t, []string{})
}

func runFixtureWithServers(t *testing.T, servers []string) (*transportFixture, string, ed25519.PrivateKey, runtoken.Claims) {
	t.Helper()
	f := newTransportFixture(t, false, false)
	f.catMu.Lock()
	for _, id := range servers {
		f.cat.MCPServerEnabled[id] = true
	}
	f.catMu.Unlock()
	if err := f.db.CreateSession(store.SessionRow{ID: "run-session", LogicalAgentID: "run-agent", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SaveSessionMCPPolicy(context.Background(), mcpgateway.SessionPolicy{SessionID: "run-session", AgentID: "run-agent", Servers: servers}.Seal()); err != nil {
		t.Fatal(err)
	}
	bearer, err := f.ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/run-session", Kind: "session", SessionID: "run-session"})
	if err != nil {
		t.Fatal(err)
	}
	var authority runtoken.Authority
	key, err := authority.Key(f.db.RunSigningKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	c := runtoken.Claims{URN: "msg://agent/agent-mux/run-agent", Session: "run-session", RunID: "run-one", JTI: "jti-one", Expires: time.Now().Add(time.Hour).Unix()}
	return f, bearer, key, c
}
func signRun(t *testing.T, key ed25519.PrivateKey, c runtoken.Claims) string {
	t.Helper()
	token, err := runtoken.Sign(key, c)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
func runHeader(token string) map[string][]string {
	return map[string][]string{runtoken.Header: {token}}
}

func TestRunTokenAdmissionAndViewBinding(t *testing.T) {
	f, bearer, key, c := runFixtureWithServers(t, []string{"keep", "removed", "tether"})
	f.catMu.Lock()
	f.cat.MCPServerEnabled["tether"] = false
	f.catMu.Unlock()
	token := signRun(t, key, c)
	response := f.request(t, http.MethodPost, "/mcp", bearer, "", runHeader(token))
	id := response.Header.Get("Mcp-Session-Id")
	status := response.StatusCode
	closeResponse(response)
	if status != 200 || id == "" {
		t.Fatalf("valid run admission: %d", status)
	}
	response = f.request(t, http.MethodPost, "/mcp", bearer, id, runHeader(token))
	status = response.StatusCode
	closeResponse(response)
	if status != 200 {
		t.Fatalf("same run: %d", status)
	}
	// Catalog narrowing preserves the saved-policy fingerprint while the signed
	// execution context remains bound to this already established connection.
	f.catMu.Lock()
	delete(f.cat.MCPServerEnabled, "removed")
	f.cat.MCPServerEnabled["keep"] = false
	f.catMu.Unlock()
	response = f.request(t, http.MethodPost, "/mcp", bearer, id, runHeader(token))
	status = response.StatusCode
	closeResponse(response)
	if status != 200 {
		t.Fatalf("same run after grant narrowing: %d", status)
	}
	other := c
	other.RunID = "run-two"
	other.JTI = "jti-two"
	for _, headers := range []map[string][]string{nil, runHeader(signRun(t, key, other))} {
		response = f.request(t, http.MethodPost, "/mcp", bearer, id, headers)
		status = response.StatusCode
		closeResponse(response)
		if status != 403 {
			t.Fatalf("view inherited changed/removed run: %d", status)
		}
	}
	// Missing header still initializes legacy sessions, but cannot join this run.
	response = f.request(t, http.MethodPost, "/mcp", bearer, "", nil)
	status = response.StatusCode
	closeResponse(response)
	if status != 200 {
		t.Fatal("legacy session refused")
	}
	wrongSession := c
	wrongSession.Session = "other"
	wrongURN := c
	wrongURN.URN = "msg://agent/forged"
	wrongTask := c
	wrongTask.Task = "invented"
	_, foreign, _ := ed25519.GenerateKey(rand.Reader)
	for name, headers := range map[string]map[string][]string{"malformed": runHeader("synthetic-secret"), "forged": runHeader(signRun(t, foreign, c)), "session": runHeader(signRun(t, key, wrongSession)), "urn": runHeader(signRun(t, key, wrongURN)), "task": runHeader(signRun(t, key, wrongTask)), "duplicate": {runtoken.Header: {token, token}}, "empty": runHeader("")} {
		t.Run(name, func(t *testing.T) {
			response := f.request(t, http.MethodPost, "/mcp", bearer, "", headers)
			defer closeResponse(response)
			b, _ := io.ReadAll(response.Body)
			if response.StatusCode != 401 || strings.Contains(string(b), token) || strings.Contains(string(b), "synthetic-secret") {
				t.Fatalf("unsafe refusal: %d", response.StatusCode)
			}
		})
	}
	// Per-method admission performs verification even without another HTTP admission.
	f.handler.mu.Lock()
	v := f.handler.sessions[id]
	f.handler.mu.Unlock()
	if _, err := f.db.DB().Exec(`UPDATE sessions SET workstream_id='changed' WHERE id='run-session'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.handler.admit(context.Background(), f.handler.viewToken(v), v.selectors); err == nil {
		t.Fatal("retained method admitted stale signed workstream")
	}
}

func TestRunTokenExpiryEndsRetainedViewAndRejectsReadmission(t *testing.T) {
	f, bearer, key, c := runFixture(t)
	c.Expires = time.Now().Add(2 * time.Second).Unix()
	token := signRun(t, key, c)
	response := f.request(t, http.MethodPost, "/mcp", bearer, "", runHeader(token))
	id := response.Header.Get("Mcp-Session-Id")
	status := response.StatusCode
	closeResponse(response)
	if status != 200 || id == "" {
		t.Fatal("short-lived run did not initialize")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.handler.mu.Lock()
		v := f.handler.sessions[id]
		f.handler.mu.Unlock()
		if v == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("monitor retained expired run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	response = f.request(t, http.MethodPost, "/mcp", bearer, id, runHeader(token))
	status = response.StatusCode
	closeResponse(response)
	if status != 401 {
		t.Fatalf("expired run re-admission: %d", status)
	}
}

func TestRunTokenAttributionSurvivesProxyEventWrite(t *testing.T) {
	for _, torque := range []string{"", "42"} {
		t.Run("torque-"+torque, func(t *testing.T) { testRunTokenProxyRow(t, torque) })
	}
}
func testRunTokenProxyRow(t *testing.T, torque string) {
	f, bearer, key, c := runFixture(t)
	if torque != "" {
		if _, err := f.db.DB().Exec(`UPDATE sessions SET run_id=? WHERE id='run-session'`, torque); err != nil {
			t.Fatal(err)
		}
	}
	token := signRun(t, key, c)
	response := f.request(t, http.MethodPost, "/mcp", bearer, "", runHeader(token))
	id := response.Header.Get("Mcp-Session-Id")
	closeResponse(response)
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"missing_run_probe","arguments":{},"_meta":{"tether.context":{"run_verified":true,"run_id":"forged"}}}}`
	req, err := http.NewRequest(http.MethodPost, daemon.BaseURL(f.addr)+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(runtoken.Header, token)
	req.Header.Set("Mcp-Session-Id", id)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	response, err = daemon.DialHTTPClient(f.addr).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	closeResponse(response)
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err := f.db.QueryProxyEvents(store.ProxyEventFilter{ToolName: "missing_run_probe"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			s := rows[0].Attribution
			if !s.Verified || !s.SessionResolved || !s.RunVerified || s.RunID != torque || s.ExecutionRunID != c.RunID || s.TokenJTI != c.JTI {
				t.Fatalf("wrong additive attribution: %+v", s)
			}
			raw, _ := json.Marshal(rows)
			if strings.Contains(string(raw), token) || strings.Contains(string(raw), bearer) {
				t.Fatal("secret persisted in proxy event")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("existing proxy_events write missing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunTokenBackendAdmissionNativeAndBearerRefresh(t *testing.T) {
	g := newBackendFixture(t)
	bearer, _ := g.mint(t, "tether", "s-rot", "tether.read")
	var authority runtoken.Authority
	key, err := authority.Key(g.f.db.RunSigningKeyPath())
	if err != nil {
		t.Fatal(err)
	}
	c := runtoken.Claims{URN: "msg://agent/agent-mux/agent", Session: "s-rot", RunID: "backend-run", JTI: "backend-jti", Expires: time.Now().Add(time.Hour).Unix()}
	token := signRun(t, key, c)
	request := func(path, bearer, run, session string) *http.Response {
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
		if strings.HasSuffix(path, "/admit") {
			body = `{}`
		}
		if session != "" {
			body = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
		}
		req, err := http.NewRequest(http.MethodPost, daemon.BaseURL(g.addr)+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if run != "" {
			req.Header.Set(runtoken.Header, run)
		}
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := daemon.DialHTTPClient(g.addr).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	for _, path := range []string{AdmissionPath, RemoteAdmissionPath} {
		resp := request(path, bearer, token, "")
		raw := requireBackendStatus(t, resp, 200)
		var admission AdmissionResponse
		if err := json.Unmarshal(raw, &admission); err != nil {
			t.Fatal(err)
		}
		if admission.Run == nil || admission.Run.JTI != c.JTI || strings.Contains(string(raw), token) {
			t.Fatal("admission lost run or exposed secret")
		}
		requireBackendStatus(t, request(path, bearer, "forged-run-secret", ""), 401)
	}
	for _, path := range []string{NativePath, RemoteNativePath} {
		resp := request(path, bearer, token, "")
		id := resp.Header.Get("Mcp-Session-Id")
		requireBackendStatus(t, resp, 200)
		if id == "" {
			t.Fatal("native session absent")
		}
		refreshed, _ := g.mint(t, "tether", "s-rot", "tether.read")
		requireBackendStatus(t, request(path, refreshed, token, id), 200)
		requireBackendStatus(t, request(path, refreshed, "", id), 403)
		requireBackendStatus(t, request(path, refreshed, "forged-run-secret", id), 401)
	}
}

func TestRunRevocationEndsRetainedView(t *testing.T) {
	for _, kind := range []string{"jti", "run"} {
		t.Run(kind, func(t *testing.T) {
			f, bearer, key, c := runFixture(t)
			token := signRun(t, key, c)
			response := f.request(t, http.MethodPost, "/mcp", bearer, "", runHeader(token))
			id := response.Header.Get("Mcp-Session-Id")
			status := response.StatusCode
			closeResponse(response)
			if status != 200 || id == "" {
				t.Fatal("run initialization failed")
			}
			stream := f.request(t, http.MethodGet, "/mcp", bearer, id, runHeader(token))
			if stream.StatusCode != http.StatusOK {
				closeResponse(stream)
				t.Fatal("run stream failed", stream.StatusCode)
			}
			defer closeResponse(stream)
			streamDone := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, stream.Body); close(streamDone) }()
			target := c.JTI
			if kind == "run" {
				target = c.RunID
			}
			if err := f.db.RevokeRunToken(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			select {
			case <-streamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("revoked run left stream open")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				f.handler.mu.Lock()
				view := f.handler.sessions[id]
				f.handler.mu.Unlock()
				if view == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("monitor retained revoked execution")
				}
				time.Sleep(20 * time.Millisecond)
			}
			response = f.request(t, http.MethodPost, "/mcp", bearer, id, runHeader(token))
			status = response.StatusCode
			closeResponse(response)
			if status != 401 {
				t.Fatalf("revoked readmission status %d", status)
			}
			if kind == "run" {
				c.JTI = "another-jti"
				response = f.request(t, http.MethodPost, "/mcp", bearer, "", runHeader(signRun(t, key, c)))
				status = response.StatusCode
				closeResponse(response)
				if status != 401 {
					t.Fatal("revoked execution accepted another jti")
				}
			}
		})
	}
}

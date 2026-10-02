package mcptransport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type lifecycleState struct {
	Names   []string
	Version int
	Fail    bool
}

// A real stdio child reads mutable fixture declarations outside protected
// catalog/state. Explicit refresh uses the actual SDK tools/list wire path.
func TestTransportLifecycleUpstreamProcess(t *testing.T) {
	dir := os.Getenv("TETHER_LIFECYCLE_FIXTURE")
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	_, _ = f.WriteString("started\n")
	_ = f.Close()
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "lifecycle-fixture", Version: "1"}, nil)
	s.AddTool(&mcpsdk.Tool{Name: "fixture_route", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	})
	s.AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			if method != "tools/list" && method != "tools/call" {
				return next(ctx, method, req)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
			if err != nil {
				return nil, err
			}
			var state lifecycleState
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
			if method == "tools/list" {
				if state.Fail {
					return nil, fmt.Errorf("fixture list refused")
				}
				tools := []*mcpsdk.Tool{}
				for _, name := range state.Names {
					tools = append(tools, &mcpsdk.Tool{Name: name, Description: fmt.Sprintf("Accepted fixture declaration %d", state.Version), InputSchema: map[string]any{"type": "object"}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}})
				}
				return &mcpsdk.ListToolsResult{Tools: tools}, nil
			}
			call := req.(*mcpsdk.CallToolRequest)
			if token := call.Params.GetProgressToken(); token != nil {
				var args struct {
					Label string `json:"label"`
				}
				if err := json.Unmarshal(call.Params.Arguments, &args); err != nil {
					return nil, err
				}
				if err := call.Session.NotifyProgress(ctx, &mcpsdk.ProgressNotificationParams{ProgressToken: token, Progress: 1, Total: 2, Message: call.Params.Name + args.Label}); err != nil {
					return nil, err
				}
				// Progress belongs to an active operation, before its final response.
				select {
				case <-time.After(50 * time.Millisecond):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: fmt.Sprintf("%s:%d", call.Params.Name, state.Version)}}}, nil
		}
	})
	if s.Run(context.Background(), &mcpsdk.StdioTransport{}) != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func lifecycleEntry(t *testing.T, id string) (config.MCPServerEntry, string) {
	t.Helper()
	if output, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--unshare-pid", "--proc", "/proc", "--dev", "/dev", "--", "true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap unavailable: %v %s", err, output)
	}
	dir := t.TempDir()
	setLifecycleState(t, dir, lifecycleState{Names: []string{id + "_echo", id + "_denied"}, Version: 1})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return config.MCPServerEntry{ID: id, Transport: "stdio", Command: executable, Args: []string{"-test.run=^TestTransportLifecycleUpstreamProcess$"}, Env: map[string]string{"TETHER_LIFECYCLE_FIXTURE": dir}}, dir
}
func setLifecycleState(t *testing.T, dir string, state lifecycleState) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path+".next", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".next", path); err != nil {
		t.Fatal(err)
	}
}

type lifecycleAuthTransport struct {
	base  http.RoundTripper
	token string
}

func (t lifecycleAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	cloned := r.Clone(r.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}
func lifecycleConnect(t *testing.T, f *transportFixture, token, mode, profile string, changed chan struct{}, progress ...chan *mcpsdk.ProgressNotificationParams) *mcpsdk.ClientSession {
	t.Helper()
	endpoint := daemon.BaseURL(f.addr) + "/mcp?discovery_mode=" + mode
	if profile != "" {
		endpoint += "&profile=" + profile
	}
	hc := daemon.DialHTTPClient(f.addr)
	hc.Timeout = 0
	if hc.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		hc.Transport = transport
	}
	hc.Transport = lifecycleAuthTransport{base: hc.Transport, token: token}
	options := &mcpsdk.ClientOptions{}
	if changed != nil {
		options.ToolListChangedHandler = func(context.Context, *mcpsdk.ToolListChangedRequest) {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
	if len(progress) > 0 {
		options.ProgressNotificationHandler = func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) { progress[0] <- req.Params }
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "lifecycle-client", Version: "1"}, options).Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
func lifecycleNames(t *testing.T, session *mcpsdk.ClientSession) []string {
	t.Helper()
	names := []string{}
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}
func lifecycleCall(t *testing.T, session *mcpsdk.ClientSession, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func lifecycleRefresh(t *testing.T, session *mcpsdk.ClientSession, id string) {
	t.Helper()
	result := lifecycleCall(t, session, "tether_catalog_refresh", map[string]any{"server": id})
	if result.IsError {
		t.Fatalf("refresh failed: %+v", result)
	}
}
func awaitLifecycleChange(t *testing.T, changed chan struct{}) {
	t.Helper()
	select {
	case <-changed:
	case <-time.After(3 * time.Second):
		t.Fatal("missing tools/list_changed")
	}
}
func drainLifecycleChanges(channels ...chan struct{}) {
	for _, channel := range channels {
		for {
			select {
			case <-channel:
				continue
			default:
			}
			break
		}
	}
}
func TestTransportLifecycleNotificationsRespectViewInventory(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprintf("unix=%t", unix), func(t *testing.T) {
			alpha, alphaDir := lifecycleEntry(t, "alpha")
			beta, betaDir := lifecycleEntry(t, "beta")
			f := newTransportFixture(t, unix, false, alpha, beta)
			f.catMu.Lock()
			f.cat.Global.MCP.Profiles["limited"] = mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"*_denied"}}}
			f.catMu.Unlock()
			a, b := f.token(t, "alpha", []string{"alpha"}), f.token(t, "beta", []string{"beta"})
			flatChange, searchChange, hiddenChange := make(chan struct{}, 32), make(chan struct{}, 32), make(chan struct{}, 32)
			flat := lifecycleConnect(t, f, a, "flat", "limited", flatChange)
			search := lifecycleConnect(t, f, a, "search", "limited", searchChange)
			hidden := lifecycleConnect(t, f, b, "flat", "", hiddenChange)
			refresher := lifecycleConnect(t, f, a, "flat", "", nil)
			if !slices.Contains(lifecycleNames(t, flat), "alpha_echo") || slices.Contains(lifecycleNames(t, flat), "alpha_denied") {
				t.Fatal("initial profile inventory wrong")
			}
			searchBefore := lifecycleNames(t, search)
			time.Sleep(40 * time.Millisecond)
			drainLifecycleChanges(flatChange, searchChange, hiddenChange)
			setLifecycleState(t, alphaDir, lifecycleState{Names: []string{"alpha_echo", "alpha_added", "alpha_denied"}, Version: 2})
			lifecycleRefresh(t, refresher, "alpha")
			awaitLifecycleChange(t, flatChange)
			awaitLifecycleChange(t, searchChange)
			if !slices.Contains(lifecycleNames(t, flat), "alpha_added") {
				t.Fatal("flat refresh missing accepted addition")
			}
			if !slices.Equal(searchBefore, lifecycleNames(t, search)) {
				t.Fatal("search protocol surface changed")
			}
			result := lifecycleCall(t, search, "tether_tool_list", map[string]any{"servers": []string{"alpha"}, "limit": 100})
			raw, _ := json.Marshal(result)
			if !strings.Contains(string(raw), "alpha_added") || strings.Contains(string(raw), "alpha_denied") {
				t.Fatal("search refresh inventory missing eligible addition or exposing denied declaration")
			}
			if slices.Contains(lifecycleNames(t, hidden), "alpha_added") {
				t.Fatal("hidden origin crossed views")
			}
			// A beta update must not signal either alpha view.
			setLifecycleState(t, betaDir, lifecycleState{Names: []string{"beta_echo", "beta_denied", "beta_added"}, Version: 2})
			time.Sleep(40 * time.Millisecond)
			drainLifecycleChanges(flatChange, searchChange, hiddenChange)
			lifecycleRefresh(t, hidden, "beta")
			awaitLifecycleChange(t, hiddenChange)
			time.Sleep(40 * time.Millisecond)
			for _, ch := range []chan struct{}{flatChange, searchChange} {
				select {
				case <-ch:
					t.Fatal("hidden origin change notified alpha view")
				default:
				}
			}
			// Change only the denied declaration; accepted eligible schemas remain exact.
			state := lifecycleState{Names: []string{"alpha_echo", "alpha_added", "alpha_new_denied"}, Version: 2}
			setLifecycleState(t, alphaDir, state)
			drainLifecycleChanges(flatChange, searchChange)
			lifecycleRefresh(t, refresher, "alpha")
			time.Sleep(60 * time.Millisecond)
			for _, ch := range []chan struct{}{flatChange, searchChange} {
				select {
				case <-ch:
					t.Fatal("denied tool change notified restricted view")
				default:
				}
			}
			for _, dir := range []string{alphaDir, betaDir} {
				raw, err := os.ReadFile(filepath.Join(dir, "starts"))
				if err != nil || strings.Count(string(raw), "started\n") != 1 {
					t.Fatalf("not one child per origin: %q %v", raw, err)
				}
			}
		})
	}
}

func TestTransportLifecycleReconnectRequiresFreshSession(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprintf("unix=%t", unix), func(t *testing.T) {
			alpha, dir := lifecycleEntry(t, "alpha")
			beta, _ := lifecycleEntry(t, "beta")
			f := newTransportFixture(t, unix, false, alpha, beta)
			f.catMu.Lock()
			f.cat.Global.MCP.Profiles["limited"] = mcpgateway.Profile{Tools: mcpgateway.ToolRules{Deny: []string{"*_denied"}}}
			f.catMu.Unlock()
			token := f.token(t, "alpha", []string{"alpha"})
			first := lifecycleConnect(t, f, token, "flat", "limited", nil)
			expected := lifecycleNames(t, first)
			if result := lifecycleCall(t, first, "alpha_echo", map[string]any{}); result.IsError {
				t.Fatal("initial call refused")
			}
			firstID := first.ID()
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			second := lifecycleConnect(t, f, token, "flat", "limited", nil)
			if firstID == second.ID() || !slices.Equal(expected, lifecycleNames(t, second)) {
				t.Fatal("new SDK session did not preserve the restricted inventory")
			}
			raw, err := os.ReadFile(filepath.Join(dir, "starts"))
			if err != nil || strings.Count(string(raw), "started\n") != 1 {
				t.Fatalf("client reconnect started another upstream: %q %v", raw, err)
			}
			staleID := second.ID()
			f.restart(t)
			response := f.request(t, http.MethodPost, "/mcp?discovery_mode=flat&profile=limited", token, staleID, nil)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusNotFound {
				t.Fatalf("stale session status %d; want 404", response.StatusCode)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err = second.CallTool(ctx, &mcpsdk.CallToolParams{Name: "alpha_echo", Arguments: map[string]any{}})
			cancel()
			if err == nil {
				t.Fatal("stale SDK session silently replayed through daemon restart")
			}
			third := lifecycleConnect(t, f, token, "flat", "limited", nil)
			if third.ID() == staleID || !slices.Equal(expected, lifecycleNames(t, third)) {
				t.Fatal("fresh initialization widened or lost the restricted inventory")
			}
			if result := lifecycleCall(t, third, "alpha_echo", map[string]any{}); result.IsError {
				t.Fatal("fresh session call refused")
			}
			raw, err = os.ReadFile(filepath.Join(dir, "starts"))
			if err != nil || strings.Count(string(raw), "started\n") != 2 {
				t.Fatalf("daemon restart did not create exactly one replacement upstream: %q %v", raw, err)
			}
		})
	}
}

func TestTransportLifecycleProgressReachesOnlyCaller(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprintf("unix=%t", unix), func(t *testing.T) {
			alpha, _ := lifecycleEntry(t, "alpha")
			f := newTransportFixture(t, unix, false, alpha)
			token := f.token(t, "alpha", []string{"alpha"})
			progress := []chan *mcpsdk.ProgressNotificationParams{make(chan *mcpsdk.ProgressNotificationParams, 8), make(chan *mcpsdk.ProgressNotificationParams, 8)}
			idleProgress := make(chan *mcpsdk.ProgressNotificationParams, 8)
			callers := []*mcpsdk.ClientSession{
				lifecycleConnect(t, f, token, "flat", "", nil, progress[0]),
				lifecycleConnect(t, f, token, "search", "", nil, progress[1]),
			}
			_ = lifecycleConnect(t, f, token, "flat", "", nil, idleProgress)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for i, caller := range callers {
				wg.Add(1)
				go func(i int, caller *mcpsdk.ClientSession) {
					defer wg.Done()
					name, args := "alpha_echo", map[string]any{"label": fmt.Sprint(i)}
					if i == 1 {
						name, args = "tether_tool_call", map[string]any{"name": "alpha_echo", "arguments": args}
					}
					_, err := caller.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args, Meta: mcpsdk.Meta{"progressToken": "same-client-token"}})
					errs <- err
				}(i, caller)
			}
			wg.Wait()
			for range callers {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			for i, ch := range progress {
				select {
				case p := <-ch:
					if p.ProgressToken != "same-client-token" || p.Message != "alpha_echo"+fmt.Sprint(i) {
						t.Fatalf("crossed progress: %+v", p)
					}
				case <-ctx.Done():
					t.Fatal("upstream progress never reached initiating view")
				}
			}
			select {
			case <-idleProgress:
				t.Fatal("progress reached idle view")
			default:
			}
		})
	}
}

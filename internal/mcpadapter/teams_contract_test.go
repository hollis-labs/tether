package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/substrate/mesh/teams/memory"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/teamcli"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

type teamCapture struct {
	method    string
	request   any
	principal identity.Principal
	calls     int
	err       error
}

func (f *teamCapture) record(ctx context.Context, method string, req any) (teamsvc.Result, error) {
	f.method, f.request = method, req
	f.principal, _ = identity.FromContext(ctx)
	f.calls++
	return teamsvc.Result{Member: &teamsvc.MemberView{ID: "member", Slot: "worker", Actor: "msg://agent/test/member", Status: "active", Kind: "agent"}, DeliveryKeys: []string{"receipt"}}, f.err
}
func (f *teamCapture) Form(ctx context.Context, r teamsvc.FormRequest) (teamsvc.Result, error) {
	return f.record(ctx, "form", r)
}
func (f *teamCapture) Dissolve(ctx context.Context, r teamsvc.RunRequest) (teamsvc.Result, error) {
	return f.record(ctx, "dissolve", r)
}
func (f *teamCapture) AddMember(ctx context.Context, r teamsvc.AddMemberRequest) (teamsvc.Result, error) {
	return f.record(ctx, "member_add", r)
}
func (f *teamCapture) RemoveMember(ctx context.Context, r teamsvc.RemoveMemberRequest) (teamsvc.Result, error) {
	return f.record(ctx, "member_remove", r)
}
func (f *teamCapture) Assign(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return f.record(ctx, "assign", r)
}
func (f *teamCapture) Delegate(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return f.record(ctx, "delegate", r)
}
func (f *teamCapture) Address(ctx context.Context, r teamsvc.MessageRequest) (teamsvc.Result, error) {
	return f.record(ctx, "address", r)
}
func (f *teamCapture) Cancel(ctx context.Context, r teamsvc.CancelRequest) (teamsvc.Result, error) {
	return f.record(ctx, "cancel", r)
}
func (f *teamCapture) ReportResult(ctx context.Context, r teamsvc.ReportResultRequest) (teamsvc.Result, error) {
	return f.record(ctx, "report_result", r)
}

var teamVerified = identity.Principal{ID: "msg://agent/test/verified", Kind: "agent"}

func teamInputs(verb string) map[string]any {
	r := map[string]any{"run_id": "run"}
	switch verb {
	case "form":
		r = map[string]any{"team": map[string]any{"id": "definition", "version": 1}, "launch": map[string]any{"counts": map[string]any{"worker": 1}}}
	case "member_add":
		r["slot"] = "worker"
		r["limits"] = map[string]any{"max_depth": 1}
	case "member_remove":
		r["member_id"] = "member"
	case "assign", "delegate", "address":
		r["address"] = "@worker"
		r["body"] = "do work"
		r["kind"] = "agent"
		r["history"] = "none"
	case "cancel":
		r["member_id"] = "member"
		r["cascade"] = true
	case "report_result":
		r["delegate_key"] = "receipt"
		r["body"] = "done"
	}
	return r
}

func teamHTTP(t *testing.T, ops api.TeamOps) *httptest.Server {
	t.Helper()
	handler := api.NewHandler(api.Deps{Teams: ops})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Test verifier accepts only its credential, never attribution headers.
		if r.Header.Get("Authorization") == "Bearer proved" {
			r = r.WithContext(identity.WithPrincipal(r.Context(), teamVerified))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}
func teamDaemonClient(server *httptest.Server) *client.Client {
	return client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("proved"))
}

func TestTeamSurfaceContract(t *testing.T) {
	t.Setenv("TETHER_TOKEN", "")
	for _, verb := range api.TeamVerbs() {
		t.Run(verb, func(t *testing.T) {
			f := &teamCapture{}
			server := teamHTTP(t, f)
			inputs := teamInputs(verb)
			raw, err := json.Marshal(inputs)
			if err != nil {
				t.Fatal(err)
			}
			var captures []any
			var outputs []json.RawMessage
			// HTTP invokes the exact handler mounted by api.NewHandler.
			req, err := http.NewRequest(http.MethodPost, server.URL+"/teams/"+verb, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer proved")
			req.Header.Set("Idempotency-Key", "same-key")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("HTTP %d %s %v", response.StatusCode, body, err)
			}
			captures = append(captures, f.request)
			outputs = append(outputs, body)
			if f.method != verb || f.principal.ID != teamVerified.ID {
				t.Fatal("HTTP service or identity mismatch", f)
			}
			// MCP uses actual registration, schema, SDK dispatch and structured result.
			a := New(nil, "test-token", []string{ScopeTeamWrite})
			a.principal = &teamVerified
			a.SetTeams(f)
			s := a.newBareServer()
			a.registerTeamTools(s)
			mcp := connectInMemory(t, s)
			result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "same-key", "request": inputs}})
			if err != nil || result.IsError {
				t.Fatalf("MCP %+v %v", result, err)
			}
			encoded, err := json.Marshal(result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			captures = append(captures, f.request)
			outputs = append(outputs, encoded)
			if f.method != verb || f.principal.ID != teamVerified.ID {
				t.Fatal("MCP service or identity mismatch", f)
			}
			// CLI executes its real command and credential-bearing daemon client.
			cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{verb, "--key", "same-key", "--request", string(raw)})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			captures = append(captures, f.request)
			outputs = append(outputs, out.Bytes())
			if f.method != verb || f.principal.ID != teamVerified.ID || f.calls != 3 {
				t.Fatal("CLI shared call or identity mismatch", f)
			}
			// Compare authored wire fields with the service request as well as
			// comparing transports, so a shared dropped field cannot pass.
			v := reflect.ValueOf(captures[0])
			if v.FieldByName("Key").String() != "same-key" {
				t.Fatal("idempotency key not passed through")
			}
			if verb != "form" && v.FieldByName("RunID").String() != inputs["run_id"] {
				t.Fatal("run id changed")
			}
			for wire, field := range map[string]string{"member_id": "MemberID", "slot": "Slot", "address": "Address", "body": "Body", "delegate_key": "DelegateKey"} {
				if expected, exists := inputs[wire]; exists && v.FieldByName(field).String() != expected {
					t.Fatal("wire field changed", wire, v.FieldByName(field))
				}
			}
			if verb == "cancel" && !v.FieldByName("Cascade").Bool() {
				t.Fatal("cascade dropped")
			}
			for i := 1; i < 3; i++ {
				if !reflect.DeepEqual(captures[0], captures[i]) {
					t.Fatalf("unequal service inputs: %+v / %+v", captures[0], captures[i])
				}
			}
			for _, output := range outputs {
				var decoded map[string]any
				if json.Unmarshal(output, &decoded) != nil {
					t.Fatal("invalid result", string(output))
				}
				var expected map[string]any
				_ = json.Unmarshal(outputs[0], &expected)
				if !reflect.DeepEqual(expected, decoded) {
					t.Fatal("different result shapes", string(output))
				}
				for _, private := range []string{"session_id", "intent", "workspace", "quota", "provision"} {
					if bytes.Contains(output, []byte(private)) {
						t.Fatal("internal response field", string(output))
					}
				}
			}
		})
	}
}

func TestTeamSurfaceErrors(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		code   string
		status int
	}{
		{teamsvc.ErrUnauthenticated, "unauthenticated", 401}, {teamsvc.ErrUnavailable, "unavailable", 503}, {teamsvc.ErrDenied, "denied", 403}, {teamsvc.ErrNotFound, "not_found", 404}, {teamsvc.ErrConflict, "conflict", 409}, {teamsvc.ErrInvalidRequest, "invalid_request", 400}, {errors.New("private-host-error"), "internal_error", 500},
	} {
		t.Run(tc.code, func(t *testing.T) {
			for _, verb := range api.TeamVerbs() {
				t.Run(verb, func(t *testing.T) {
					f := &teamCapture{err: fmt.Errorf("private cause: %w", tc.cause)}
					server := teamHTTP(t, f)
					inputs := teamInputs(verb)
					raw, _ := json.Marshal(inputs)
					req, _ := http.NewRequest(http.MethodPost, server.URL+"/teams/"+verb, bytes.NewReader(raw))
					req.Header.Set("Idempotency-Key", "key")
					response, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					var envelope api.ErrorResponse
					_ = json.Unmarshal(body, &envelope)
					if response.StatusCode != tc.status || envelope.Error.Code != tc.code || bytes.Contains(body, []byte("private")) {
						t.Fatal("HTTP error mapping", response.StatusCode, string(body))
					}
					a := New(nil, "test-token", []string{ScopeTeamWrite})
					a.SetTeams(f)
					s := a.newBareServer()
					a.registerTeamTools(s)
					mcp := connectInMemory(t, s)
					result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "key", "request": inputs}})
					if err != nil || !result.IsError {
						t.Fatalf("MCP error missing %+v %v", result, err)
					}
					encoded, _ := json.Marshal(result.StructuredContent)
					var detail struct {
						Code string `json:"code"`
					}
					_ = json.Unmarshal(encoded, &detail)
					if detail.Code != tc.code || bytes.Contains(encoded, []byte("private")) {
						t.Fatal("MCP mapping", string(encoded))
					}
					cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
					cmd.SetOut(io.Discard)
					cmd.SetErr(io.Discard)
					cmd.SetArgs([]string{verb, "--key", "key", "--request", string(raw)})
					err = cmd.Execute()
					var typed *client.TeamError
					if !errors.As(err, &typed) || typed.Code != tc.code || typed.Status != tc.status || strings.Contains(err.Error(), "private") {
						t.Fatal("CLI mapping", err)
					}
				})
			}
		})
	}
}

func TestTeamDisabledSurfaces(t *testing.T) {
	server := teamHTTP(t, nil)
	for _, verb := range api.TeamVerbs() {
		response, err := http.Post(server.URL+"/teams/"+verb, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 404 {
			t.Fatal("disabled route registered", response.StatusCode)
		}
		cmd := &cobra.Command{Use: "tether"}
		cmd.AddCommand(&cobra.Command{Use: "existing", Run: func(*cobra.Command, []string) {}})
		if team := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, false); team != nil {
			cmd.AddCommand(team)
		}
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"team", verb})
		err = cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatal("disabled CLI verb exists", err)
		}
	}
	// An existing endpoint remains available with teams absent.
	response, err := http.Get(server.URL + "/auth/context")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("existing endpoint changed", response.StatusCode)
	}
	a := New(nil, "test-token", []string{ScopeTeamWrite})
	s := a.newBareServer()
	a.registerTeamTools(s)
	a.registerHealthTools(s)
	mcp := connectInMemory(t, s)
	tools, err := mcp.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.HasPrefix(tool.Name, "tether_team_") {
			t.Fatal("disabled MCP team tool listed", tool.Name)
		}
	}
	foundHealth := false
	for _, tool := range tools.Tools {
		if tool.Name == "tether_health" {
			foundHealth = true
		}
	}
	if !foundHealth {
		t.Fatal("existing health tool changed")
	}
}

// These ports deliberately cannot perform effects. Authentication must finish
// before the journal and run lookup on every verb.
type refusalPorts struct{ calls int }

func (p *refusalPorts) GetRun(context.Context, string) (teams.TeamRun, error) {
	return teams.TeamRun{}, teamsvc.ErrNotFound
}
func (p *refusalPorts) GetOrCreate(context.Context, teamhost.Intent) (teamhost.Record, error) {
	p.calls++
	return teamhost.Record{}, teamsvc.ErrDenied
}
func (*refusalPorts) SetPlan(context.Context, teamhost.Scope, json.RawMessage) error {
	return teamsvc.ErrDenied
}
func (*refusalPorts) Complete(context.Context, teamhost.Scope, json.RawMessage) error {
	return teamsvc.ErrDenied
}
func (*refusalPorts) FormationCeilings(context.Context, teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	return teamsvc.ConservativePolicy(), nil
}

type operatorProofKey struct{}
type surfacePrincipal struct {
	calls int
	seen  string
}

// Only host-installed context identity is evidence. Request claims are ignored.
func (p *surfacePrincipal) ResolvePrincipal(ctx context.Context) (teamsvc.Principal, error) {
	p.calls++
	principal, verified := identity.FromContext(ctx)
	p.seen = principal.ID
	local, _ := ctx.Value(operatorProofKey{}).(bool)
	kind := mesh.ActorAgent
	if principal.Kind == "user" {
		kind = mesh.ActorUser
	}
	return teamsvc.Principal{ID: mesh.URN(principal.ID), Kind: kind, Verified: verified, LocalOperator: local && principal.ID == string(teamsvc.LocalOperator)}, nil
}

type teamVerifier struct{}

func (teamVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	switch token {
	case "proved":
		return teamVerified, nil
	case "operator":
		return identity.Principal{ID: string(teamsvc.LocalOperator), Kind: "user"}, nil
	default:
		return identity.Principal{}, identity.ErrInvalidToken
	}
}

func realRefusalService(t *testing.T, p *surfacePrincipal) (*teamsvc.Service, *refusalPorts) {
	t.Helper()
	h := memory.New()
	ports := &refusalPorts{}
	svc, err := teamsvc.New(teamsvc.Deps{Principals: p, Ceilings: ports, Runs: ports, Calls: ports, Definitions: h, Roster: h, Ledger: h, Signals: h, Provisioner: h, Workflows: h, Triggers: h, Sender: h, Routing: h, Trust: h, Approvals: h, Clock: h, IDs: h, Defaults: teamsvc.ConservativePolicy().Limits})
	if err != nil {
		t.Fatal(err)
	}
	return svc, ports
}

func TestTeamSurfacesRefuseUnverifiedPrincipals(t *testing.T) {
	t.Setenv("TETHER_TOKEN", "")
	for _, mode := range []string{"unverified", "asserted headers", "asserted query", "bad bearer", "verified", "local operator", "operator without proof"} {
		t.Run(mode, func(t *testing.T) {
			for _, verb := range api.TeamVerbs() {
				t.Run(verb, func(t *testing.T) {
					p := &surfacePrincipal{}
					svc, ports := realRefusalService(t, p)
					target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						// A host-proved transport marker is independent of all request fields.
						if mode == "local operator" {
							r = r.WithContext(context.WithValue(r.Context(), operatorProofKey{}, true))
						}
						api.NewHandler(api.Deps{Teams: svc}).ServeHTTP(w, r)
					})
					// Observe carries only verified credentials and lets the real service
					// itself prove that anonymous and asserted identities cannot act.
					server := httptest.NewServer(identity.Middleware(identity.Observe, teamVerifier{}, nil, target))
					defer server.Close()
					inputs := teamInputs(verb)
					raw, _ := json.Marshal(inputs)
					wanted := 401
					token := ""
					if mode == "bad bearer" || mode == "asserted headers" {
						token = "forged"
					}
					if mode == "verified" {
						token = "proved"
						wanted = 404
					}
					if mode == "local operator" {
						token = "operator"
						wanted = 404
					}
					if mode == "operator without proof" {
						token = "operator"
					}
					if wanted == 404 && verb == "form" {
						wanted = 403
					}
					path := server.URL + "/teams/" + verb
					if mode == "asserted query" {
						path += "?as=msg://user/local/operator"
					}
					req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
					req.Header.Set("Idempotency-Key", "key")
					if token != "" {
						req.Header.Set("Authorization", "Bearer "+token)
					}
					req.Header.Set("X-Tether-Principal", string(teamsvc.LocalOperator))
					req.Header.Set("X-Tether-Verified", "true")
					response, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if response.StatusCode != wanted {
						t.Fatal("HTTP refusal", response.StatusCode, wanted, string(body))
					}
					if wanted == 401 && mode != "operator without proof" && p.seen != "" {
						t.Fatal("asserted identity reached context", p.seen)
					}
					a := New(nil, "scope-credential", []string{ScopeTeamWrite})
					a.SetTeams(svc)
					// Native dispatch alone installs verified context; claims cannot do so.
					if mode == "verified" {
						principal := teamVerified
						a.principal = &principal
					}
					if mode == "local operator" || mode == "operator without proof" {
						principal := identity.Principal{ID: string(teamsvc.LocalOperator), Kind: "user"}
						a.principal = &principal
					}
					s := a.newBareServer()
					if mode == "local operator" {
						s.SDKServer().AddReceivingMiddleware(func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
							return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
								return next(context.WithValue(ctx, operatorProofKey{}, true), method, req)
							}
						})
					}
					a.registerTeamTools(s)
					mcp := connectInMemory(t, s)
					result, callErr := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "key", "request": inputs}})
					if callErr != nil || !result.IsError {
						t.Fatalf("MCP refusal %+v %v", result, callErr)
					}
					encoded, _ := json.Marshal(result.StructuredContent)
					_, detail := api.TeamError(map[int]error{401: teamsvc.ErrUnauthenticated, 403: teamsvc.ErrDenied, 404: teamsvc.ErrNotFound}[wanted])
					if !bytes.Contains(encoded, []byte(detail.Code)) {
						t.Fatal("MCP classification", string(encoded), detail.Code)
					}
					for _, args := range []map[string]any{
						{"key": "key", "request": inputs, "as": string(teamsvc.LocalOperator)},
						{"key": "key", "request": map[string]any{"run_id": "run", "as": string(teamsvc.LocalOperator)}},
					} {
						result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: args})
						if err != nil || !result.IsError {
							t.Fatal("MCP asserted identity accepted", err)
						}
						encoded, _ := json.Marshal(result.StructuredContent)
						if !bytes.Contains(encoded, []byte("invalid_request")) {
							t.Fatal("MCP asserted identity", string(encoded))
						}
					}
					clientURL := server.URL
					if mode == "asserted headers" {
						destination, _ := url.Parse(server.URL)
						proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
							pr.SetURL(destination)
							pr.Out.Header.Set("Authorization", "Bearer forged")
							pr.Out.Header.Set("X-Tether-Principal", string(teamsvc.LocalOperator))
							pr.Out.Header.Set("X-Tether-Verified", "true")
						}}
						forgingClient := httptest.NewServer(proxy)
						defer forgingClient.Close()
						clientURL = forgingClient.URL
					}
					cmd := teamcli.NewCommand(func() (*client.Client, error) {
						return client.New("tcp:"+strings.TrimPrefix(clientURL, "http://"), client.WithToken(token)), nil
					}, true)
					cmd.SetOut(io.Discard)
					cmd.SetErr(io.Discard)
					cmd.SetArgs([]string{verb, "--key", "key", "--request", string(raw)})
					err = cmd.Execute()
					var failure *client.TeamError
					if !errors.As(err, &failure) || failure.Status != wanted {
						t.Fatal("CLI refusal", err, wanted)
					}
					if p.calls != 3 {
						t.Fatal("surfaces did not reach the same service resolver", p.calls)
					}
					if wanted == 401 && ports.calls != 0 {
						t.Fatal("unauthenticated reached journal", ports.calls)
					}
				})
			}
		})
	}
}

func TestTeamSurfaceBoundsAndAssertedBody(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"identity claim", `{"run_id":"run","principal":"msg://user/local/operator","verified":true}`},
		{"operator claim", `{"run_id":"run","local_operator":true}`},
		{"raw request", `{"run_id":"run","address":"` + strings.Repeat("b", teamsvc.MaxRequestBytes) + `"}`},
		{"raw body", `{"run_id":"run","address":"@worker","body":"` + strings.Repeat("b", teamsvc.MaxBodyBytes+1) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &teamCapture{}
			server := teamHTTP(t, f)
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/teams/address", strings.NewReader(tc.body))
			req.Header.Set("Idempotency-Key", "key")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatal("HTTP invalid bounds/claim", response.StatusCode)
			}
			var inputs map[string]any
			if json.Unmarshal([]byte(tc.body), &inputs) != nil {
				t.Fatal("bad fixture")
			}
			a := New(nil, "test-token", []string{ScopeTeamWrite})
			a.SetTeams(f)
			_, err = a.handleTeam(context.Background(), "address", map[string]any{"key": "key", "request": inputs})
			if err == nil || !strings.Contains(err.Error(), "invalid_request") {
				t.Fatal("MCP bounds/claim accepted", err)
			}
			cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"address", "--key", "key", "--request", tc.body})
			if err := cmd.Execute(); err == nil {
				t.Fatal("CLI bounds/claim accepted")
			}
			if f.calls != 0 {
				t.Fatal("invalid input reached service", f.calls)
			}
		})
	}
}

func TestTeamNilAndNativeRegistration(t *testing.T) {
	for _, ops := range []api.TeamOps{nil, (*teamsvc.Service)(nil), &teamCapture{}} {
		enabled := api.HasTeamOps(ops)
		handler := api.NewHandler(api.Deps{Teams: ops})
		for _, verb := range api.TeamVerbs() {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				req := httptest.NewRequest(method, "/teams/"+verb, strings.NewReader("{}"))
				req.Header.Set("Idempotency-Key", "key")
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, req)
				if !enabled && w.Code != 404 {
					t.Fatal("nil route present", verb, w.Code)
				}
			}
		}
		a := New(nil, "scope-credential", []string{ScopeTeamWrite})
		a.SetTeams(ops)
		mcp := connectInMemory(t, a.newServer())
		tools, err := mcp.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, verb := range api.TeamVerbs() {
			count := 0
			for _, tool := range tools.Tools {
				if tool.Name == "tether_team_"+verb {
					count++
				}
			}
			wanted := 0
			if enabled {
				wanted = 1
			}
			if count != wanted {
				t.Fatal("native team registration", verb, count, wanted)
			}
		}
	}
}

func TestTeamPOSTOnly(t *testing.T) {
	f := &teamCapture{}
	handler := api.NewHandler(api.Deps{Teams: f})
	for _, verb := range api.TeamVerbs() {
		for _, method := range []string{"GET", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"} {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(method, "/teams/"+verb, strings.NewReader("{}")))
			if w.Code != 405 || w.Header().Get("Allow") != "POST" {
				t.Fatal("method guard", verb, method, w.Code, w.Header())
			}
		}
	}
	if f.calls != 0 {
		t.Fatal("wrong method invoked service")
	}
}

func TestTeamMCPScopeAndLegacyTrace(t *testing.T) {
	if ScopeTeamWrite != "team.write" {
		t.Fatal("public scope changed", ScopeTeamWrite)
	}
	for _, tc := range []struct {
		token  string
		scopes []string
		code   string
	}{
		{"", []string{ScopeTeamWrite}, "auth_required"}, {"credential", nil, "insufficient_scope"}, {"credential", []string{ScopeTeamWrite}, ""},
	} {
		for _, verb := range api.TeamVerbs() {
			f := &teamCapture{}
			a := New(nil, tc.token, tc.scopes)
			a.SetTeams(f)
			mcp := connectInMemory(t, a.newServer())
			result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "key", "request": teamInputs(verb), "_traceparent": "", "_tracestate": ""}})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result.StructuredContent)
			if tc.code != "" {
				if !result.IsError || !bytes.Contains(encoded, []byte(tc.code)) || f.calls != 0 {
					t.Fatal("scope bypass", verb, string(encoded), f.calls)
				}
			} else if result.IsError || f.calls != 1 {
				t.Fatal("allowed call refused", verb, string(encoded))
			}
		}
	}
}

func TestTeamSurfaceVerbFieldsAndKeys(t *testing.T) {
	for _, verb := range api.TeamVerbs() {
		for _, tc := range []struct {
			name, key string
			extra     bool
		}{
			{"field", "key", true}, {"leading", " key", false}, {"trailing", "key ", false}, {"newline", "key\n", false}, {"nul", "key\x00", false},
		} {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				inputs := teamInputs(verb)
				if tc.extra {
					extra := "member_id"
					if verb == "member_remove" || verb == "cancel" {
						extra = "slot"
					}
					if verb == "member_add" {
						extra = "cascade"
					}
					inputs[extra] = "unexpected"
				}
				raw, _ := json.Marshal(inputs)
				f := &teamCapture{}
				handler := api.NewHandler(api.Deps{Teams: f})
				request := httptest.NewRequest("POST", "/teams/"+verb, bytes.NewReader(raw))
				request.Header.Set("Idempotency-Key", tc.key)
				w := httptest.NewRecorder()
				handler.ServeHTTP(w, request)
				if w.Code != 400 {
					t.Fatal("HTTP validation", w.Code, w.Body.String())
				}
				a := New(nil, "credential", []string{ScopeTeamWrite})
				a.SetTeams(f)
				s := a.newBareServer()
				a.registerTeamTools(s)
				mcp := connectInMemory(t, s)
				result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": tc.key, "request": inputs}})
				if err != nil || !result.IsError {
					t.Fatal("MCP validation", err)
				}
				encoded, _ := json.Marshal(result.StructuredContent)
				if !bytes.Contains(encoded, []byte("invalid_request")) {
					t.Fatal(string(encoded))
				}
				factories := 0
				cmd := teamcli.NewCommand(func() (*client.Client, error) { factories++; return nil, errors.New("should not construct client") }, true)
				cmd.SetOut(io.Discard)
				cmd.SetErr(io.Discard)
				cmd.SetArgs([]string{verb, "--key", tc.key, "--request", string(raw)})
				err = cmd.Execute()
				var failure *client.TeamError
				if !errors.As(err, &failure) || failure.Code != "invalid_request" || factories != 0 || f.calls != 0 {
					t.Fatal("CLI validation", err, factories, f.calls)
				}
			})
		}
	}
}

func TestTeamCLIErrorOutput(t *testing.T) {
	server := teamHTTP(t, &teamCapture{err: teamsvc.ErrUnauthenticated})
	root := &cobra.Command{Use: "tether"}
	root.AddCommand(teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"team", "dissolve", "--key", "key", "--request", `{"run_id":"run"}`})
	err := root.Execute()
	var failure *client.TeamError
	if !errors.As(err, &failure) || failure.Code != "unauthenticated" || output.Len() != 0 {
		t.Fatal("root should return one cause-free error for main to print", err, output.String())
	}
}

// A fresh, populated run makes missing-field checks exercise the real library,
// rather than failing earlier because membership or run metadata is absent.
type requiredRun struct{ run teams.TeamRun }

func (r requiredRun) GetRun(_ context.Context, id string) (teams.TeamRun, error) {
	if id != r.run.ID {
		return teams.TeamRun{}, teamsvc.ErrNotFound
	}
	return r.run, nil
}

type requiredCalls struct{ plans, results int }

func (*requiredCalls) GetOrCreate(_ context.Context, i teamhost.Intent) (teamhost.Record, error) {
	return teamhost.Record{Intent: i}, nil
}
func (c *requiredCalls) SetPlan(context.Context, teamhost.Scope, json.RawMessage) error {
	c.plans++
	return nil
}
func (c *requiredCalls) Complete(context.Context, teamhost.Scope, json.RawMessage) error {
	c.results++
	return nil
}

func populatedTeamService(t *testing.T) (*teamsvc.Service, *memory.Host, *requiredCalls) {
	t.Helper()
	h := memory.New()
	ctx := context.Background()
	limits := teamsvc.ConservativePolicy().Limits
	definition := teams.Team{ID: "definition", Version: 1, Name: "Required fields", Slots: []teams.Slot{{Name: "owner", Resolution: teams.Durable, Identity: mesh.URN(teamVerified.ID), Activation: teams.Singleton, Min: 1, Max: 1}},
		Phases:    []teams.Phase{{ID: "work", Kind: "flex", ActiveSlots: []string{"owner"}, OwnerSlot: "owner", ExitTrigger: teams.Trigger{Kind: "event", Spec: map[string]string{"event": "done"}}}},
		Authority: teams.Authority{Mode: teams.Strict, Grants: []teams.Grant{{FromSlot: "owner", ToSlot: "owner", Verb: teams.MayMessage}, {FromSlot: "owner", ToSlot: "owner", Verb: teams.MayAssign}, {FromSlot: "owner", ToSlot: "owner", Verb: teams.MayDelegate}}},
		Policy:    teams.Policy{Spawn: limits}, Routing: teams.Routing{CoordinatorSlot: "owner"}}
	if err := h.PutDefinition(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := h.Mutate(ctx, "run", func(r *teams.Roster) error {
		*r = teams.Roster{RunID: "run", Members: []teams.Member{{ID: "owner", Slot: "owner", Actor: mesh.URN(teamVerified.ID), Kind: mesh.ActorAgent, Status: "active", Governance: teams.Owner, Resolution: teams.Durable, Limits: limits, Budget: limits.Budget, SpawnCapable: true, Enrolled: true}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := &requiredCalls{}
	svc, err := teamsvc.New(teamsvc.Deps{Principals: &surfacePrincipal{}, Ceilings: &refusalPorts{}, Runs: requiredRun{teams.TeamRun{ID: "run", TeamID: definition.ID, TeamVersion: 1, Status: mesh.TaskWorking, Channel: "team.run"}}, Calls: calls, Definitions: h, Roster: h, Ledger: h, Signals: h, Provisioner: h, Workflows: h, Triggers: h, Sender: h, Routing: h, Trust: h, Approvals: h, Clock: h, IDs: h, Defaults: limits})
	if err != nil {
		t.Fatal(err)
	}
	return svc, h, calls
}

func TestTeamRealServiceMissingFields(t *testing.T) {
	for _, tc := range []struct {
		verb    string
		request api.TeamRequest
		status  int
	}{
		{"member_remove", api.TeamRequest{RunID: "run"}, 404}, {"cancel", api.TeamRequest{RunID: "run"}, 404},
		{"member_add", api.TeamRequest{RunID: "run"}, 404}, {"report_result", api.TeamRequest{RunID: "run"}, 400},
		{"dissolve", api.TeamRequest{}, 400},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			svc, h, calls := populatedTeamService(t)
			before, err := h.Snapshot(context.Background(), "run")
			if err != nil {
				t.Fatal(err)
			}
			_, err = api.CallTeam(identity.WithPrincipal(context.Background(), teamVerified), svc, tc.verb, "key", tc.request)
			var typed *teamsvc.Error
			status, _ := api.TeamError(err)
			if !errors.As(err, &typed) || status != tc.status {
				t.Fatal("real service missing field", err, status, tc.status)
			}
			after, err := h.Snapshot(context.Background(), "run")
			if err != nil || !reflect.DeepEqual(before, after) || calls.plans != 0 || calls.results != 0 {
				t.Fatal("missing field had an effect", err, calls)
			}
		})
	}
	t.Run("empty address defaults to coordinator in library", func(t *testing.T) {
		svc, _, calls := populatedTeamService(t)
		_, err := api.CallTeam(identity.WithPrincipal(context.Background(), teamVerified), svc, "address", "key", api.TeamRequest{RunID: "run", Body: "work", Kind: mesh.ActorAgent, History: "none"})
		if err != nil || calls.results != 1 {
			t.Fatal("library empty address behavior", err, calls)
		}
	})
}

func TestTeamSurfacesRequireFields(t *testing.T) {
	for _, verb := range api.TeamVerbs() {
		for _, field := range api.TeamRequiredFields(verb) {
			t.Run(verb+"/"+field, func(t *testing.T) {
				for _, empty := range []bool{false, true} {
					inputs := teamInputs(verb)
					if empty {
						if field == "team" {
							inputs[field] = nil
						} else {
							inputs[field] = ""
						}
					} else {
						delete(inputs, field)
					}
					raw, _ := json.Marshal(inputs)
					svc, h, calls := populatedTeamService(t)
					before, _ := h.Snapshot(context.Background(), "run")
					server := teamHTTP(t, svc)
					req, _ := http.NewRequest("POST", server.URL+"/teams/"+verb, bytes.NewReader(raw))
					req.Header.Set("Idempotency-Key", "key")
					req.Header.Set("Authorization", "Bearer proved")
					response, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					if response.StatusCode != 400 {
						t.Fatal("HTTP missing required", response.StatusCode)
					}
					a := New(nil, "credential", []string{ScopeTeamWrite})
					p := teamVerified
					a.principal = &p
					a.SetTeams(svc)
					s := a.newBareServer()
					a.registerTeamTools(s)
					mcp := connectInMemory(t, s)
					result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "key", "request": inputs}})
					if err != nil || !result.IsError {
						t.Fatal("MCP missing required", err)
					}
					encoded, _ := json.Marshal(result.StructuredContent)
					if !bytes.Contains(encoded, []byte("invalid_request")) {
						t.Fatal(string(encoded))
					}
					cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
					cmd.SetOut(io.Discard)
					cmd.SetErr(io.Discard)
					cmd.SetArgs([]string{verb, "--key", "key", "--request", string(raw)})
					err = cmd.Execute()
					var failure *client.TeamError
					if !errors.As(err, &failure) || failure.Code != "invalid_request" {
						t.Fatal("CLI missing required", err)
					}
					after, _ := h.Snapshot(context.Background(), "run")
					if !reflect.DeepEqual(before, after) || calls.plans != 0 || calls.results != 0 {
						t.Fatal("missing field changed real service state", calls)
					}
				}
			})
		}
	}
}

func TestTeamSurfaceCaseVariantFields(t *testing.T) {
	for _, verb := range api.TeamVerbs() {
		t.Run(verb, func(t *testing.T) {
			inputs := teamInputs(verb)
			field := api.TeamRequiredFields(verb)[0]
			inputs[strings.ToUpper(field)] = inputs[field]
			delete(inputs, field)
			raw, _ := json.Marshal(inputs)
			f := &teamCapture{}
			server := teamHTTP(t, f)
			req, _ := http.NewRequest("POST", server.URL+"/teams/"+verb, bytes.NewReader(raw))
			req.Header.Set("Idempotency-Key", "key")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if response.StatusCode != 400 {
				t.Fatal("HTTP case variant accepted", response.StatusCode)
			}
			a := New(nil, "credential", []string{ScopeTeamWrite})
			a.SetTeams(f)
			_, err = a.handleTeam(context.Background(), verb, map[string]any{"key": "key", "request": inputs})
			if err == nil {
				t.Fatal("MCP case variant accepted")
			}
			cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{verb, "--key", "key", "--request", string(raw)})
			err = cmd.Execute()
			var failure *client.TeamError
			if !errors.As(err, &failure) || failure.Code != "invalid_request" || f.calls != 0 {
				t.Fatal("case variant invoked service", err, f.calls)
			}
		})
	}
}

func TestTeamInvalidUTF8Key(t *testing.T) {
	key := string([]byte{0xff})
	f := &teamCapture{}
	handler := api.NewHandler(api.Deps{Teams: f})
	req := httptest.NewRequest("POST", "/teams/dissolve", strings.NewReader(`{"run_id":"run"}`))
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatal("HTTP invalid UTF-8", w.Code)
	}
	// JSON encoders replace invalid string bytes before RPC. Exercise the handler
	// directly so this probe reaches the actual raw-key guard rather than a codec.
	a := New(nil, "credential", []string{ScopeTeamWrite})
	a.SetTeams(f)
	_, err := a.handleTeam(context.Background(), "dissolve", map[string]any{"key": key, "request": map[string]any{"run_id": "run"}})
	if err == nil || !strings.Contains(err.Error(), "invalid_request") {
		t.Fatal("MCP invalid UTF-8", err)
	}
	cmd := teamcli.NewCommand(func() (*client.Client, error) { t.Fatal("invalid key constructed client"); return nil, nil }, true)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"dissolve", "--key", key, "--request", `{"run_id":"run"}`})
	err = cmd.Execute()
	var failure *client.TeamError
	if !errors.As(err, &failure) || failure.Code != "invalid_request" || f.calls != 0 {
		t.Fatal("CLI invalid UTF-8", err, f.calls)
	}
}

func TestTeamSurfacesDefaultAddress(t *testing.T) {
	for _, verb := range []string{"assign", "delegate", "address"} {
		for _, explicitEmpty := range []bool{false, true} {
			for _, surface := range []string{"HTTP", "MCP", "CLI"} {
				t.Run(fmt.Sprintf("%s/%t/%s", verb, explicitEmpty, surface), func(t *testing.T) {
					svc, _, calls := populatedTeamService(t)
					inputs := teamInputs(verb)
					if explicitEmpty {
						inputs["address"] = ""
					} else {
						delete(inputs, "address")
					}
					raw, _ := json.Marshal(inputs)
					server := teamHTTP(t, svc)
					var result teamsvc.Result
					switch surface {
					case "HTTP":
						req, _ := http.NewRequest("POST", server.URL+"/teams/"+verb, bytes.NewReader(raw))
						req.Header.Set("Idempotency-Key", "key")
						req.Header.Set("Authorization", "Bearer proved")
						response, err := http.DefaultClient.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = response.Body.Close() }()
						body, _ := io.ReadAll(response.Body)
						if response.StatusCode != 200 || json.Unmarshal(body, &result) != nil {
							t.Fatal("HTTP coordinator route", response.StatusCode, string(body))
						}
					case "MCP":
						a := New(nil, "credential", []string{ScopeTeamWrite})
						p := teamVerified
						a.principal = &p
						a.SetTeams(svc)
						s := a.newBareServer()
						a.registerTeamTools(s)
						mcp := connectInMemory(t, s)
						response, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_" + verb, Arguments: map[string]any{"key": "key", "request": inputs}})
						if err != nil || response.IsError {
							t.Fatalf("MCP coordinator route %+v %v", response, err)
						}
						encoded, _ := json.Marshal(response.StructuredContent)
						if json.Unmarshal(encoded, &result) != nil {
							t.Fatal(string(encoded))
						}
					case "CLI":
						cmd := teamcli.NewCommand(func() (*client.Client, error) { return teamDaemonClient(server), nil }, true)
						var output bytes.Buffer
						cmd.SetOut(&output)
						cmd.SetErr(io.Discard)
						cmd.SetArgs([]string{verb, "--key", "key", "--request", string(raw)})
						if err := cmd.Execute(); err != nil || json.Unmarshal(output.Bytes(), &result) != nil {
							t.Fatal("CLI coordinator route", err, output.String())
						}
					}
					if len(result.Recipients) != 1 || string(result.Recipients[0].Actor) != teamVerified.ID || calls.results != 1 {
						t.Fatal("coordinator fallback did not reach real library", result, calls)
					}
				})
			}
		}
	}
}

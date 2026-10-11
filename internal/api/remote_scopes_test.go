package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// This checks the executable registrations, not a mirrored route count or
// document. A newly registered route must have an explicit policy before it
// can be admitted remotely; optional dependencies do not hide registrations.
func TestRemoteScopesCoverRegisteredRoutes(t *testing.T) {
	policy := map[string]bool{}
	for _, rule := range RemoteRouteScopes {
		policy[rule.Registration] = true
	}
	for _, pattern := range registeredRoutePatterns(t) {
		if !policy[pattern] {
			t.Errorf("registered route %s lacks explicit remote policy", pattern)
		}
	}
	if policy["/future/unclassified"] {
		t.Fatal("unknown fixture unexpectedly classified")
	}
	request := httptest.NewRequest("GET", "/future/unclassified", nil)
	if _, _, known := RequiredRemoteScope(request); known {
		t.Fatal("unknown registration admitted")
	}
}

// registeredRoutePatterns returns every ServeMux pattern the api package
// registers, parsed from source, plus the team verbs. Shared by the remote
// and local table coverage tests.
func registeredRoutePatterns(t *testing.T) []string {
	t.Helper()
	var patterns []string
	check := func(pattern string) {
		if _, path, ok := strings.Cut(pattern, " "); ok {
			pattern = path
		}
		patterns = append(patterns, pattern)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			registered := false
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				registered = fn.Sel.Name == "Handle" || fn.Sel.Name == "HandleFunc"
			case *ast.Ident:
				registered = fn.Name == "get" && name == "docs.go"
			}
			if !registered {
				return true
			}
			if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				pattern, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatal(err)
				}
				check(pattern)
			} else if name != "docs.go" && name != "teams.go" {
				t.Errorf("unclassified dynamic route registration in %s", name)
			}
			return true
		})
	}
	for _, verb := range TeamVerbs() {
		check("/teams/" + verb)
	}
	return patterns
}

func TestRemoteScopesEnforceIndependentAndPayloadCases(t *testing.T) {
	for _, tc := range []struct {
		method, path, body string
		scopes             []string
		want               int
	}{
		{"GET", "/sessions", "", []string{"read"}, 204},
		{"POST", "/sessions", `{"launch":"demo"}`, []string{"read"}, 403},
		{"POST", "/sessions", `{"launch":"demo"}`, []string{"operate"}, 204},
		{"POST", "/sessions", `{"launch":"demo","agent_file":"/tmp/custom"}`, []string{"operate"}, 403},
		{"POST", "/sessions", `{"launch":"demo","agent_file":"/tmp/custom"}`, []string{"operate", "maintain"}, 204},
		{"POST", "/sessions", `{"launch":"demo","injection":"{}"}`, []string{"operate"}, 403},
		{"POST", "/sessions/s/input", "text", []string{"operate"}, 403},
		{"POST", "/sessions/s/input", "text", []string{"terminal"}, 204},
		{"GET", "/sessions/s/attach", "", []string{"read"}, 403},
		{"GET", "/sessions/s/attach", "", []string{"terminal"}, 204},
		{"POST", "/messages", `{}`, []string{"admin"}, 403},
		{"GET", "/auth/devices", "", []string{"admin"}, 204},
		{"POST", "/auth/pair", `{}`, []string{"admin"}, 403},
		{"POST", "/auth/pair/exchange", `{}`, nil, 204},
		{"GET", "/auth/pair/exchange", "", []string{"admin"}, 404},
		{"GET", "/registry/agents/msg%3A%2F%2Fagent%2Flocal%2Ffixture", "", []string{"read"}, 204},
		{"GET", "/registry/new-kind", "", []string{"read"}, 404},
		{"POST", "/a2a/agents/x/rpc", `{"method":"GetTask"}`, []string{"read"}, 204},
		{"POST", "/a2a/agents/x/rpc", `{"method":"SendMessage"}`, []string{"read"}, 403},
		{"POST", "/a2a/agents/x/rpc", `{"method":"SendMessage"}`, []string{"operate"}, 204},
		{"POST", "/a2a/agents/x/rpc", `{"method":"FutureMethod"}`, []string{"admin", "operate", "read", "maintain"}, 403},
		{"GET", "/future", "", []string{"read"}, 404},
		{"DELETE", "/sessions", "", []string{"operate"}, 404},
	} {
		t.Run(tc.method+tc.path+strings.Join(tc.scopes, ","), func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			ctx := identity.WithRemoteContext(r.Context())
			ctx = identity.WithPrincipal(ctx, identity.Principal{Verified: true, ID: "msg://device/test", Kind: "device", Scopes: tc.scopes})
			w := httptest.NewRecorder()
			RemoteScopeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r.WithContext(ctx))
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
		})
	}
	// Existing local operator policy bypasses the remote scope table.
	w := httptest.NewRecorder()
	RemoteScopeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, httptest.NewRequest("POST", "/sessions", nil))
	if w.Code != 204 {
		t.Fatal("local behavior changed")
	}
}

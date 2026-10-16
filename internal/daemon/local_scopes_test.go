package daemon

import (
	"context"
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Every route the local mux can register (daemon-level literals, the device
// auth paths, and every api mount with all dependencies enabled) must have a
// LocalRouteScopes registration. A new mount without one fails here.
func TestLocalScopeTableCoversLocalMux(t *testing.T) {
	policy := map[string]bool{}
	for _, rule := range api.LocalRouteScopes {
		policy[rule.Registration] = true
	}
	need := func(pattern, from string) {
		t.Helper()
		if !policy[pattern] {
			t.Errorf("%s route %s lacks a local scope table entry", from, pattern)
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Handle" || sel.Sel.Name == "HandleFunc") && len(x.Args) > 0 {
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					pattern, _ := strconv.Unquote(lit.Value)
					need(pattern, "server.go")
				}
			}
		case *ast.BasicLit:
			// The device auth paths are registered from a string slice.
			if x.Kind == token.STRING {
				if v, _ := strconv.Unquote(x.Value); strings.HasPrefix(v, "/auth/") {
					need(v, "server.go")
				}
			}
		}
		return true
	})
	// apiMounts lists every path regardless of which dependencies are enabled.
	// A subtree mount is covered by entries beneath it (/teams/ -> /teams/form).
	for _, m := range (&Server{}).apiMounts() {
		covered := policy[m.path]
		for _, rule := range api.LocalRouteScopes {
			covered = covered || strings.HasSuffix(m.path, "/") && strings.HasPrefix(rule.Path, m.path)
		}
		if !covered {
			t.Errorf("apiMounts route %s lacks a local scope table entry", m.path)
		}
	}
}

// Acceptance 2: a raw request carrying a valid principal token, with no MCP
// adapter involved, is attributed to principal + route + required scope, and
// observe mode never rejects it.
func TestLocalScopeAttributionRawRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	token, err := ids.Mint(context.Background(), identity.Principal{ID: "msg://service/local/curl", Kind: "service", Display: "curl", Scopes: []string{"read"}, Addresses: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: ids}
	defer s.CloseIdentityAudit()
	for _, tc := range []struct{ method, path, pattern, scope string }{
		{"POST", "/sessions/s1/input?x=1", "/sessions/{id}/input", "input"},
		{"GET", "/sessions/s1/events", "/sessions/{id}/events", "read"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		if out.Code == http.StatusForbidden || out.Code == http.StatusUnauthorized {
			t.Fatalf("observe rejected %s: %d", tc.path, out.Code)
		}
		want := 0 // the service principal holds only "read"
		if tc.scope == "read" {
			want = 1
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			var principal, pattern, scope string
			var allow sql.NullInt64
			err := db.DB().QueryRow(`SELECT principal_id,route_pattern,required_scope,would_allow FROM identity_audit WHERE route_pattern=?`, tc.pattern).Scan(&principal, &pattern, &scope, &allow)
			if err == nil {
				if principal != "msg://service/local/curl" || scope != tc.scope || !allow.Valid || int(allow.Int64) != want {
					t.Fatalf("wrong attribution %s %s %v", principal, scope, allow)
				}
				break
			}
			if err != sql.ErrNoRows || time.Now().After(deadline) {
				t.Fatal("audit not persisted", err)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

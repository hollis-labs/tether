package daemon

// route_mount_test.go — CW-20260912-0059 regression.
//
// WHAT SHIPPED BROKEN. internal/api registered /workstreams on its own mux,
// cmd/mux/daemon.go wired Deps.Workstreams, and every handler was correct. The
// outer mux in Handler() never routed the path, so the entire S1 HTTP surface
// 404'd in production while 1732 tests passed.
//
// WHY EVERY TEST MISSED IT. They all build api.NewHandler directly. Nothing
// exercised daemon.Server.Handler(), which is the only thing production
// serves. whoami_route_test.go had already documented this exact class and
// tested for it — and did not catch this, because a per-route test only
// protects the route someone remembered to write one for.
//
// SO THIS IS STRUCTURAL, NOT BEHAVIORAL. An earlier draft probed each path
// over HTTP through a fully-wired Server. That needed every optional
// dependency stubbed, and a nil dependency made an unmounted path
// indistinguishable from a deliberately-disabled one — reintroducing the exact
// confound that hid the bug. Comparing the two lists needs no dependencies at
// all, covers every path regardless of guard, and fails for the right reason.

import (
	"sort"
	"strings"
	"testing"
)

// apiTopLevelPaths is every top-level path internal/api registers on its own
// mux. Maintained by hand because Go's ServeMux exposes no way to enumerate
// registered patterns; regenerate with:
//
//	grep -rhoE 'mux\.HandleFunc\("(/[^"]*)"' internal/api/*.go |
//	  sed -E 's/.*"(\/[^"]*)"/\1/' | sort -u
var apiTopLevelPaths = []string{
	"/ai/audit", "/ai/budgets", "/ai/chat", "/ai/chat/stream", "/ai/embeddings",
	"/ai/models", "/ai/providers", "/ai/routes", "/ai/routes/explain",
	"/ai/routes/preview", "/ai/usage",
	"/broker/envelopes", "/broker/envelopes/", "/broker/requests",
	"/catalog/agents", "/catalog/launches", "/catalog/projects", "/catalog/providers",
	"/events", "/events/stream",
	"/fs/detect", "/fs/validate",
	"/groups", "/groups/",
	"/logical-agents", "/logical-agents/",
	"/logs/daemon",
	"/mentions",
	"/messages", "/messages/", "/messages/inbox", "/messages/list",
	"/messages/notify", "/messages/request", "/messages/retention/candidates",
	"/messages/subscribe", "/messages/thread/",
	"/proxy/events",
	"/registry/", "/registry/bindings", "/registry/bindings/", "/registry/bootstrap",
	"/registry/scoped-bindings", "/registry/scoped-bindings/resolve",
	"/registry/scoped-bindings/revisions",
	"/session-groups", "/session-groups/",
	"/sessions", "/sessions/", "/sessions/bootstrap",
	"/whoami",
	"/workstreams", "/workstreams/",
}

// coveredBySubtree reports whether an exact path is already served because a
// parent subtree pattern is mounted. ServeMux does longest-prefix matching, so
// mounting "/registry/" genuinely serves "/registry/bindings" — those paths do
// not each need their own entry, and demanding one would be noise rather than
// safety.
func coveredBySubtree(path string, mounted map[string]bool) bool {
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '/' {
			continue
		}
		if prefix := path[:i+1]; prefix != path && mounted[prefix] {
			return true
		}
	}
	return false
}

// The guard. Every path api owns must be routable by the daemon — mounted
// outright, or covered by a mounted subtree.
//
// Guards are ignored on purpose: this asks whether the path is in the
// allowlist at all, not whether this particular Server has the dependency to
// serve it. "Not in the list" is the failure that shipped.
func TestAPIMounts_CoverEveryAPIPath(t *testing.T) {
	var s Server
	mounted := map[string]bool{}
	for _, m := range s.apiMounts() {
		mounted[m.path] = true
	}

	var missing []string
	for _, path := range apiTopLevelPaths {
		if mounted[path] || coveredBySubtree(path, mounted) {
			continue
		}
		missing = append(missing, path)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("internal/api registers these paths and Handler() routes none of them, so they 404 in production while every api-level test passes:\n  %s\n\nAdd them to (*Server).apiMounts.",
			strings.Join(missing, "\n  "))
	}
}

// The reverse direction: a mount for a path api does not register is dead
// weight, and usually means a path was renamed on one side only.
func TestAPIMounts_HaveNoStrayEntries(t *testing.T) {
	owned := map[string]bool{}
	for _, p := range apiTopLevelPaths {
		owned[p] = true
	}

	var stray []string
	var s Server
	for _, m := range s.apiMounts() {
		if !owned[m.path] {
			stray = append(stray, m.path)
		}
	}
	if len(stray) > 0 {
		sort.Strings(stray)
		t.Errorf("Handler() routes paths internal/api does not register:\n  %s\n\nEither api dropped them or the two lists drifted.",
			strings.Join(stray, "\n  "))
	}
}

// The specific route that shipped unreachable, named so the regression has an
// obvious home even if the lists above are edited.
func TestAPIMounts_IncludeWorkstreams(t *testing.T) {
	var s Server
	want := map[string]bool{"/workstreams": false, "/workstreams/": false}
	for _, m := range s.apiMounts() {
		if _, ok := want[m.path]; ok {
			want[m.path] = true
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("%s is not in apiMounts; S1's HTTP surface is unreachable through the real daemon", path)
		}
	}
}

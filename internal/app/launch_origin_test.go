package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/launch"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func ceilingService(t *testing.T) (*Service, *identity.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Service{Store: db}, identity.NewStore(db.DB())
}

// mintSessionWithScopes gives a session a credential with exactly these scopes,
// then optionally ends it, as a parent or a resumed source would be.
func mintSessionWithScopes(t *testing.T, svc *Service, ids *identity.Store, id string, ended bool, scopes ...string) {
	t.Helper()
	if err := svc.Store.CreateSession(store.SessionRow{ID: id, LaunchID: "l", ProjectID: "p", ProviderID: "claude", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ids.MintSessionForLaunch(context.Background(), identity.Principal{ID: "msg://session/local/" + id, Kind: "session", SessionID: id, Scopes: scopes}); err != nil {
		t.Fatal(err)
	}
	if ended {
		if err := ids.RevokeSession(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLaunchCeilingBoundsByLauncherAndByOriginAndSaysWhatVouched(t *testing.T) {
	svc, ids := ceilingService(t)
	mintSessionWithScopes(t, svc, ids, "parent", false, "a", "b")
	mintSessionWithScopes(t, svc, ids, "ended-source", true, "b", "c") // credential revoked: its grant still bounds a resume
	mintSessionWithScopes(t, svc, ids, "wild-parent", false, "*")
	granted := []string{"a", "b", "c", "d"}
	launcher := func(scopes ...string) context.Context {
		return identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://agent/test/l", Kind: "agent", Scopes: scopes})
	}
	for name, tc := range map[string]struct {
		ctx   context.Context
		want  []string
		attrb Attribution
	}{
		"verified launcher, subset":         {launcher("a", "d"), []string{"a", "d"}, AttributionPrincipal},
		"verified launcher, wildcard":       {launcher("*"), granted, AttributionPrincipal},
		"team child of a scoped parent":     {withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "parent"}), []string{"a", "b"}, AttributionOrigin},
		"team child of a wildcard parent":   {withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "wild-parent"}), granted, AttributionOrigin},
		"resume of an ended session":        {withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginResume, OriginSessionID: "ended-source"}), []string{"b", "c"}, AttributionOrigin},
		"origin session with no credential": {withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginResume, OriginSessionID: "never-had-one"}), granted, AttributionInternalUnattributed},
		"internal launch with no origin":    {withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam}), granted, AttributionInternalUnattributed},
		"launcher AND origin both bound it": {withLaunchOrigin(launcher("a", "b", "c"), InternalLaunch{Kind: OriginResume, OriginSessionID: "ended-source"}), []string{"b", "c"}, AttributionPrincipal},
		"anonymous external":                {context.Background(), granted, AttributionAnonymous},
	} {
		t.Run(name, func(t *testing.T) {
			got, attribution, err := svc.launchCeiling(tc.ctx, granted)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) || attribution != tc.attrb {
				t.Fatalf("got %v / %s, want %v / %s", got, attribution, tc.want, tc.attrb)
			}
		})
	}
}

// On a worker, a team child is bounded by its parent session's grant, through
// the same helper the hub path uses.
func TestWorkerTeamChildIsBoundedByItsParent(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	one := launchprofile.DefaultMCPScopes[0]
	mintSessionWithScopes(t, r.svc, r.ids, "team-parent", false, one)
	spec, err := r.launchAs(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "team-parent"}), "team-child", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, []string{one}) || !slices.Equal(r.storedScopes("team-child"), []string{one}) {
		t.Fatalf("child scopes: spec %v stored %v, want only %v", spec.MCPScopes, r.storedScopes("team-child"), one)
	}
	// A child of a parent with no recorded credential keeps the profile ceiling.
	spec, err = r.launchAs(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "no-such-parent"}), "orphan-child", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, launchprofile.DefaultMCPScopes) {
		t.Fatalf("unattributed internal child scopes %v", spec.MCPScopes)
	}
}

// The internal-origin marker must be unreachable from anything that serves a
// request. The key and the setter are unexported, so other packages cannot
// touch them; this test also keeps LaunchInternal, the one exported way to set
// it, out of the request-serving packages.
func TestOnlyDaemonCompositionCanSetALaunchOrigin(t *testing.T) {
	banned := map[string]bool{"LaunchInternal": true, "InternalLaunch": true, "withLaunchOrigin": true, "launchOriginKey": true}
	for _, dir := range []string{"../api", "../mcpadapter", "../daemon", "../acpadapter", "../acpsvc", "../client", "../a2a", "../mcpgateway"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && banned[id.Name] {
					t.Errorf("%s/%s references %s: a request-serving package must not start internal launches", dir, e.Name(), id.Name)
				}
				return true
			})
		}
	}
	if _, err := os.ReadDir("../api"); err != nil {
		t.Fatalf("boundary test is not looking at the api package: %v", err)
	}
}

// For the same origin, the worker path is never broader than the hub path, and
// neither exceeds the origin's grant (architect: ruling item 4). Scopes that
// both profiles name must come out the same on both.
func TestWorkerPathIsNeverBroaderThanTheHubPathForTheSameOrigin(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	common := "message.write" // in both workerScopes (hub) and DefaultMCPScopes (worker)
	if !slices.Contains(workerScopes, common) || !slices.Contains(launchprofile.DefaultMCPScopes, common) {
		t.Fatalf("test assumes %q is in both profiles", common)
	}
	for name, grant := range map[string][]string{
		"origin holds the common scope":   {common},
		"origin holds an unrelated one":   {"unrelated.scope"},
		"origin holds a wildcard":         {"*"},
		"origin holds two profile scopes": {common, "registry.write"},
	} {
		t.Run(name, func(t *testing.T) {
			origin := "origin-" + strings.ReplaceAll(name, " ", "-")
			mintSessionWithScopes(t, r.svc, r.ids, origin, false, grant...)
			ctx := withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: origin})

			// Hub-local path.
			hubID := "hub-" + origin
			if err := r.svc.Store.CreateSession(store.SessionRow{ID: hubID, LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "created"}, &launch.Plan{LaunchID: "pm", ProjectID: "tether", ProviderID: "claude"}); err != nil {
				t.Fatal(err)
			}
			if _, err := r.svc.mintSessionCredential(ctx, hubID); err != nil {
				t.Fatal(err)
			}
			hub := r.storedScopes(hubID)

			// Worker path.
			spec, err := r.launchAs(ctx, "worker-"+origin, "")
			if err != nil {
				t.Fatal(err)
			}
			worker := r.storedScopes("worker-" + origin)
			if !slices.Equal(spec.MCPScopes, worker) {
				t.Fatalf("worker spec %v and credential %v disagree", spec.MCPScopes, worker)
			}
			if !slices.Contains(grant, "*") {
				for _, scope := range append(slices.Clone(hub), worker...) {
					if !slices.Contains(grant, scope) {
						t.Fatalf("scope %q exceeds the origin's grant %v (hub %v, worker %v)", scope, grant, hub, worker)
					}
				}
			}
			if slices.Contains(hub, common) != slices.Contains(worker, common) {
				t.Fatalf("hub %v and worker %v disagree about the shared scope %q for the same origin", hub, worker, common)
			}
		})
	}
}

// An origin whose grant cannot be read refuses the launch; it never falls back
// to the profile ceiling. This holds on both credential paths and in any
// identity mode.
func TestAnUnreadableOriginGrantRefusesTheLaunch(t *testing.T) {
	svc, ids := ceilingService(t)
	mintSessionWithScopes(t, svc, ids, "corrupt-origin", false, "a")
	if _, err := svc.Store.DB().Exec(`UPDATE principals SET scopes_json='{not json' WHERE session_id='corrupt-origin'`); err != nil {
		t.Fatal(err)
	}
	ctx := withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "corrupt-origin"})
	got, _, err := svc.launchCeiling(ctx, []string{"a", "b"})
	if !errors.Is(err, ErrLaunchRefused) || got != nil {
		t.Fatalf("corrupt origin grant: got %v, %v; want a refusal", got, err)
	}
	// Hub-local credential path.
	if err = svc.Store.CreateSession(store.SessionRow{ID: "child", LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "created"}, &launch.Plan{LaunchID: "pm", ProjectID: "tether", ProviderID: "claude"}); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.mintSessionCredential(ctx, "child"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("hub path minted a credential for an unreadable origin: %v", err)
	}
	var n int
	if err = svc.Store.DB().QueryRow(`SELECT COUNT(*) FROM principals WHERE session_id='child'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a credential exists for the refused launch: %d %v", n, err)
	}
	// A closed database is an error too, not a pass.
	closed, cids := ceilingService(t)
	mintSessionWithScopes(t, closed, cids, "o", false, "a")
	_ = closed.Store.Close()
	if _, _, err = closed.launchCeiling(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginResume, OriginSessionID: "o"}), []string{"a"}); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("closed store: %v", err)
	}
}

// Worker path: the same refusal, and no credential is left behind.
func TestWorkerLaunchWithAnUnreadableOriginIsRefused(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	mintSessionWithScopes(t, r.svc, r.ids, "bad-parent", false, "a")
	if _, err := r.db.DB().Exec(`UPDATE principals SET scopes_json='{not json' WHERE session_id='bad-parent'`); err != nil {
		t.Fatal(err)
	}
	_, err := r.launchAs(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "bad-parent"}), "worker-child", "")
	if !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("worker launch with an unreadable origin: %v", err)
	}
}

// An origin that never had a credential keeps the profile ceiling by design
// (legacy parity: it predates session credentials or identity was off); the
// result says so.
func TestAnOriginThatNeverHadACredentialKeepsTheProfileCeilingAndSaysSo(t *testing.T) {
	svc, _ := ceilingService(t)
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })
	got, attribution, err := svc.launchCeiling(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginResume, OriginSessionID: "pre-credentials"}), []string{"a", "b"})
	if err != nil || !slices.Equal(got, []string{"a", "b"}) || attribution != AttributionInternalUnattributed {
		t.Fatalf("got %v / %s / %v", got, attribution, err)
	}
	if !strings.Contains(logs.String(), "pre-credentials") {
		t.Fatalf("no log line for the unbounded fallback: %q", logs.String())
	}
}

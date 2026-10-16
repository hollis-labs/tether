package app

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type enforceRig struct {
	*identityRig
	root  string
	clock time.Time
}

// newEnforceRig points the service at a catalog whose global.yaml the test can
// rewrite, with a controllable clock for the persisted-mode cache.
func newEnforceRig(t *testing.T, identityYAML string) *enforceRig {
	t.Helper()
	r := &enforceRig{identityRig: newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred), root: t.TempDir(), clock: time.Unix(1_000_000, 0)}
	r.svc.CatalogRoot = r.root
	r.svc.persisted.now = func() time.Time { return r.clock }
	r.persist(identityYAML)
	return r
}

func (r *enforceRig) persist(identityYAML string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.root, "global.yaml"), []byte(identityYAML), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

// pastTheTTL advances the clock so the next launch re-reads global.yaml.
func (r *enforceRig) pastTheTTL() { r.clock = r.clock.Add(persistedIdentityTTL + time.Second) }

func (r *enforceRig) credentials(id string) int {
	r.t.Helper()
	var n int
	if err := r.db.DB().QueryRow(`SELECT COUNT(*) FROM principals WHERE session_id=?`, id).Scan(&n); err != nil {
		r.t.Fatal(err)
	}
	return n
}

func (r *enforceRig) hubMint(ctx context.Context, id string) error {
	r.t.Helper()
	if err := r.svc.Store.CreateSession(store.SessionRow{ID: id, LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "created"}, &launch.Plan{LaunchID: "pm", ProjectID: "tether", ProviderID: "claude"}); err != nil {
		r.t.Fatal(err)
	}
	_, err := r.svc.mintSessionCredential(ctx, id)
	return err
}

var (
	operatorCtx   = identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	internalNoOrg = withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam})
)

// Once the PERSISTED mode is enforce, an unbounded launch is refused without a
// restart, on both credential paths, and leaves no credential behind; a bounded
// launch is untouched.
func TestPersistedEnforceRefusesUnboundedLaunchesBeforeRestart(t *testing.T) {
	r := newEnforceRig(t, "identity:\n  mode: observe\n")
	mintSessionWithScopes(t, r.svc, r.ids, "origin", false, "message.write")
	// Observe: unbounded launches are allowed, as before.
	if _, err := r.launchAs(context.Background(), "w-anon-1", ""); err != nil {
		t.Fatalf("observe, anonymous worker launch: %v", err)
	}
	if err := r.hubMint(context.Background(), "h-anon-1"); err != nil {
		t.Fatalf("observe, anonymous hub launch: %v", err)
	}
	// The operator flips to enforce. The running daemon has not restarted.
	r.persist("identity:\n  mode: enforce\n")
	r.pastTheTTL()
	for name, ctx := range map[string]context.Context{"anonymous": context.Background(), "internal_unattributed": internalNoOrg} {
		if _, err := r.launchAs(ctx, "w-"+name, ""); !errors.Is(err, ErrLaunchRefused) {
			t.Errorf("worker %s launch under persisted enforce: %v", name, err)
		}
		if err := r.hubMint(ctx, "h-"+name); !errors.Is(err, ErrLaunchRefused) {
			t.Errorf("hub %s launch under persisted enforce: %v", name, err)
		}
		if r.credentials("w-"+name) != 0 || r.credentials("h-"+name) != 0 {
			t.Errorf("%s: a credential exists for a refused launch", name)
		}
	}
	// Bounded launches are not affected.
	if _, err := r.launchAs(operatorCtx, "w-bounded", ""); err != nil {
		t.Errorf("principal-bounded worker launch refused: %v", err)
	}
	if err := r.hubMint(operatorCtx, "h-bounded"); err != nil {
		t.Errorf("principal-bounded hub launch refused: %v", err)
	}
	if _, err := r.launchAs(withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: "origin"}), "w-origin", ""); err != nil {
		t.Errorf("origin-bounded worker launch refused: %v", err)
	}
}

// Which persisted setting triggers which refusal.
func TestPersistedEnforceTriggersPerAttribution(t *testing.T) {
	for name, tc := range map[string]struct {
		identityYAML           string
		anonRefused, unattrRef bool
	}{
		"observe everywhere":                {"identity:\n  mode: observe\n", false, false},
		"only teams enforce":                {"identity:\n  mode: observe\n  points:\n    teams: enforce\n", false, false},
		"only mcp_gateway enforce":          {"identity:\n  mode: observe\n  points:\n    mcp_gateway: enforce\n", false, false},
		"only local_api enforce":            {"identity:\n  mode: observe\n  points:\n    local_api: enforce\n", true, false},
		"global enforce":                    {"identity:\n  mode: enforce\n", true, true},
		"global enforce, local_api observe": {"identity:\n  mode: enforce\n  points:\n    local_api: observe\n", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newEnforceRig(t, tc.identityYAML)
			_, anonErr := r.launchAs(context.Background(), "anon", "")
			_, unattrErr := r.launchAs(internalNoOrg, "unattr", "")
			if (errors.Is(anonErr, ErrLaunchRefused)) != tc.anonRefused || (anonErr != nil && !tc.anonRefused) {
				t.Errorf("anonymous: %v, refused want %v", anonErr, tc.anonRefused)
			}
			if (errors.Is(unattrErr, ErrLaunchRefused)) != tc.unattrRef || (unattrErr != nil && !tc.unattrRef) {
				t.Errorf("internal_unattributed: %v, refused want %v", unattrErr, tc.unattrRef)
			}
		})
	}
}

// The persisted mode is cached for a few seconds, then re-read.
func TestPersistedModeIsCachedForTheTTLThenReread(t *testing.T) {
	r := newEnforceRig(t, "identity:\n  mode: observe\n")
	if _, err := r.launchAs(context.Background(), "a1", ""); err != nil {
		t.Fatal(err)
	}
	r.persist("identity:\n  mode: enforce\n")
	if _, err := r.launchAs(context.Background(), "a2", ""); err != nil {
		t.Fatalf("a flip inside the TTL window was acted on early: %v", err)
	}
	if r.svc.persisted.hits != 1 {
		t.Fatalf("%d catalog reads inside the TTL, want 1", r.svc.persisted.hits)
	}
	r.pastTheTTL()
	if _, err := r.launchAs(context.Background(), "a3", ""); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("after the TTL the flip was not seen: %v", err)
	}
	if r.svc.persisted.hits != 2 {
		t.Fatalf("%d catalog reads, want 2", r.svc.persisted.hits)
	}
}

// If the catalog cannot be read when an unbounded launch is decided, it is
// refused; a bounded launch never reads the catalog and still works.
func TestUnreadableCatalogRefusesUnboundedLaunchesAndSparesBoundedOnes(t *testing.T) {
	r := newEnforceRig(t, "identity:\n  mode: observe\n")
	// A global.yaml that exists but cannot be read (here: it is a directory).
	if err := os.Remove(filepath.Join(r.root, "global.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(r.root, "global.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	r.pastTheTTL()
	// (The worker path trips over the same unreadable file earlier, reading its
	// MCP settings, with a different error; either way the launch fails and no
	// credential is minted. The hub path reaches the rule itself.)
	for _, id := range []string{"anon", "unattr"} {
		if _, err := r.launchAs(context.Background(), "w-"+id, ""); err == nil || r.credentials("w-"+id) != 0 {
			t.Fatalf("worker launch %s with an unreadable catalog: %v", id, err)
		}
	}
	if err := r.hubMint(context.Background(), "anon"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("anonymous hub launch with an unreadable catalog: %v", err)
	}
	if err := r.hubMint(internalNoOrg, "unattr"); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("internal_unattributed hub launch with an unreadable catalog: %v", err)
	}
	hits := r.svc.persisted.hits
	if err := r.hubMint(operatorCtx, "bounded"); err != nil {
		t.Fatalf("bounded launch with an unreadable catalog: %v", err)
	}
	if r.svc.persisted.hits != hits {
		t.Fatal("a bounded launch read the catalog")
	}
	// A garbled catalog fails closed the same way.
	if err := os.Remove(filepath.Join(r.root, "global.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "global.yaml"), []byte("identity: [not, a, mapping"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.pastTheTTL()
	// (The worker path trips over the garbled file earlier, reading its MCP
	// settings; either way the launch fails and no credential is minted.)
	if _, err := r.launchAs(context.Background(), "anon2", ""); err == nil || r.credentials("anon2") != 0 {
		t.Fatalf("anonymous launch with a garbled catalog: %v, %d credentials", err, r.credentials("anon2"))
	}
	if err := r.hubMint(context.Background(), "anon3"); !errors.Is(err, ErrLaunchRefused) || r.credentials("anon3") != 0 {
		t.Fatalf("anonymous hub launch with a garbled catalog: %v", err)
	}
}

// With no global.yaml at the root at all (an in-process service built from an
// in-memory catalog) there is no persisted mode to consult; the mode the daemon
// started with still counts, and enforce at start is never loosened.
func TestNoGlobalYamlFallsBackToTheStartMode(t *testing.T) {
	r := newEnforceRig(t, "identity:\n  mode: observe\n")
	if err := os.Remove(filepath.Join(r.root, "global.yaml")); err != nil {
		t.Fatal(err)
	}
	r.pastTheTTL()
	var logs strings.Builder
	orig := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(orig) })
	if _, err := r.launchAs(context.Background(), "observe-start", ""); err != nil {
		t.Fatalf("no global.yaml, observe at start: %v", err)
	}
	if !strings.Contains(logs.String(), "has no global.yaml") {
		t.Fatalf("a missing global.yaml was not warned about: %q", logs.String())
	}
	r.pastTheTTL()
	_, _ = r.launchAs(context.Background(), "again", "")
	if strings.Count(logs.String(), "has no global.yaml") != 1 {
		t.Fatalf("the warning repeated: %q", logs.String())
	}
	r.svc.Catalog = &config.Catalog{}
	r.svc.Catalog.Global.Identity.Mode = "enforce"
	if _, err := r.launchAs(context.Background(), "enforce-start", ""); !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("enforce at start was loosened by a missing global.yaml: %v", err)
	}
}

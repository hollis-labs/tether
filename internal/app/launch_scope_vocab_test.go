package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func principalCtx(kind string, scopes ...string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "msg://" + kind + "/test/p", Kind: kind, TokenID: "jti", Scopes: scopes})
}

// CW-20261020-0048: a Tachyon device (bare operate+maintain+read) launching a
// general worker must keep the default profile scopes. The ceiling is the
// profile's legacy names; the launcher holds bare device scopes.
func TestWorkerNarrowingKeepsAuthorizedScopesAcrossVocabularies(t *testing.T) {
	defaults := launchprofile.DefaultMCPScopes // message.write, registry.write, groups.write
	for name, tc := range map[string]struct {
		ctx  context.Context
		want []string
	}{
		"device operate+maintain+read (the Tachyon device)": {principalCtx("device", "maintain", "operate", "read"), defaults},
		"device operate alone: messages, groups":            {principalCtx("device", "operate"), []string{"message.write", "groups.write"}},
		"device maintain alone: registry":                   {principalCtx("device", "maintain"), []string{"registry.write"}},
		"device admin alone: registry only":                 {principalCtx("device", "admin"), []string{"registry.write"}},
		"device read, terminal, worker: nothing":            {principalCtx("device", "read", "terminal", "worker"), nil},
		"device read alone: nothing":                        {principalCtx("device", "read"), nil},
		"canonical tether.* scopes":                         {principalCtx("agent", "tether.messages", "tether.registry", "tether.groups"), defaults},
		"canonical operate+maintain":                        {principalCtx("agent", "tether.operate", "tether.maintain"), defaults},
		"canonical, one capability missing":                 {principalCtx("agent", "tether.messages", "tether.registry"), []string{"message.write", "registry.write"}},
		"legacy child scopes, exact":                        {principalCtx("session", "message.write", "groups.write"), []string{"message.write", "groups.write"}},
		"tether.* wildcard":                                 {principalCtx("agent", "tether.*"), defaults},
		"another app's wildcard grants nothing":             {principalCtx("agent", "tachyon.*"), nil},
		"star":                                              {principalCtx("operator", "*"), defaults},
		"unrelated scope":                                   {principalCtx("device", "unrelated.scope"), nil},
		"no scopes":                                         {principalCtx("device"), nil},
		"no principal keeps the profile":                    {context.Background(), defaults},
	} {
		t.Run(name, func(t *testing.T) {
			if got := narrowToLauncherWorker(tc.ctx, defaults); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The narrowing only ever keeps an entry of the profile ceiling that the
// launcher already holds. It never adds, reorders into new names, unions or
// widens: checked over every launcher scope set against every scope name in
// both vocabularies, and the one-way rule (a legacy credential does not gain
// canonical authority) is asserted explicitly.
func TestWorkerNarrowingNeverWidens(t *testing.T) {
	names := []string{"read", "operate", "terminal", "maintain", "admin", "worker", "events",
		"tether.read", "tether.operate", "tether.terminal", "tether.maintain", "tether.admin", "tether.worker",
		"tether.registry", "tether.catalog", "tether.sessions", "tether.messages", "tether.groups", "tether.teams", "tether.ai", "tether.events",
		"session.write", "message.write", "registry.write", "groups.write", "catalog.write", "ai.invoke", "team.write",
		"tether.*", "tachyon.*", "unrelated.scope"}
	for _, a := range names {
		for _, b := range names {
			held := []string{a, b}
			for _, ceiling := range [][]string{{"message.write", "registry.write", "groups.write"}, {"ai.invoke", "team.write", "session.write"}, {"tether.messages", "tether.sessions"}, nil} {
				got := narrowToLauncherWorker(principalCtx("device", held...), ceiling)
				for _, scope := range got {
					if !slices.Contains(ceiling, scope) {
						t.Fatalf("held %v: %q is not in the ceiling %v (widened)", held, scope, ceiling)
					}
					if !slices.Contains(held, scope) && !identity.ScopeGranted(held, scope) {
						t.Fatalf("held %v: kept %q, which the launcher does not hold", held, scope)
					}
				}
				if len(got) > len(ceiling) {
					t.Fatalf("held %v: %v is longer than the ceiling %v", held, got, ceiling)
				}
			}
		}
	}
	// Mutation guard: operate holds ai.invoke, but a ceiling without it never gains it.
	if got := narrowToLauncherWorker(principalCtx("device", "operate", "maintain"), []string{"message.write"}); !slices.Equal(got, []string{"message.write"}) {
		t.Fatalf("operate+maintain turned a one-scope ceiling into %v", got)
	}
	// One way: legacy names do not satisfy a canonical ceiling.
	if got := narrowToLauncherWorker(principalCtx("session", "message.write", "registry.write"), []string{"tether.messages", "tether.registry"}); got != nil {
		t.Fatalf("legacy scopes gained canonical authority: %v", got)
	}
}

// The hub-local path is unchanged: a device's child gets no legacy scopes there
// (docs/device-pairing.md); only issued agent/session credentials compare across vocabularies.
func TestHubLocalNarrowingKeepsDevicesStrict(t *testing.T) {
	granted := []string{"message.write", "registry.write", "groups.write"}
	if got := narrowToLauncher(principalCtx("device", "maintain", "operate", "read"), granted); got != nil {
		t.Fatalf("hub-local device child inherited %v", got)
	}
	if got := narrowToLauncher(principalCtx("agent", "tether.operate", "tether.maintain"), granted); !slices.Equal(got, granted) {
		t.Fatalf("issued agent credential: got %v, want %v", got, granted)
	}
}

// An internal-origin ceiling (team child, resume) compares across vocabularies
// the same way, and stays a bound: it can only remove.
func TestOriginCeilingComparesAcrossVocabularies(t *testing.T) {
	svc, ids := ceilingService(t)
	mintSessionWithScopes(t, svc, ids, "canon-origin", false, "tether.messages", "tether.registry")
	mintSessionWithScopes(t, svc, ids, "legacy-origin", false, "message.write")
	granted := []string{"message.write", "registry.write", "groups.write"}
	for name, tc := range map[string]struct {
		origin string
		want   []string
	}{
		"canonical origin, legacy ceiling": {"canon-origin", []string{"message.write", "registry.write"}},
		"legacy origin, legacy ceiling":    {"legacy-origin", []string{"message.write"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := withLaunchOrigin(context.Background(), InternalLaunch{Kind: OriginTeam, OriginSessionID: tc.origin})
			got, _, err := svc.launchCeiling(ctx, granted)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	// An origin grant on a device-style launcher cannot widen the launcher's own bound.
	ctx := withLaunchOrigin(principalCtx("device", "operate"), InternalLaunch{Kind: OriginTeam, OriginSessionID: "canon-origin"})
	got, _, err := svc.launchCeilingFor(ctx, granted, narrowToLauncherWorker)
	if err != nil || !slices.Equal(got, []string{"message.write"}) {
		t.Fatalf("launcher operate AND origin canonical: got %v, %v; want only message.write", got, err)
	}
}

// A worker launch whose launcher holds none of the profile's scopes is a launch
// refusal with its own code, not an internal error, and no credential or
// worker launch comes out of it.
func TestWorkerLaunchWithNoUsableScopesIsAnActionableRefusal(t *testing.T) {
	r := newIdentityRig(t, "bind", "deliver", workerwire.FeatureSessionCred)
	_, err := r.launchAs(principalCtx("device", "read"), "sess-no-scopes", "")
	if !errors.Is(err, ErrLaunchRefused) {
		t.Fatalf("want a launch refusal, got %v", err)
	}
	var coded interface{ ErrorCode() string }
	if !errors.As(err, &coded) || coded.ErrorCode() != "launch_scope_unavailable" {
		t.Fatalf("refusal carries no launch_scope_unavailable code: %v", err)
	}
	msg := err.Error()
	for _, leak := range []string{"msg://", "jti", "Bearer", "tth_"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("refusal leaks %q: %s", leak, msg)
		}
	}
	for _, scope := range launchprofile.DefaultMCPScopes {
		if !strings.Contains(msg, scope) {
			t.Fatalf("refusal does not name the needed scope %q: %s", scope, msg)
		}
	}
	var n int
	if err := r.db.DB().QueryRow(`SELECT COUNT(*) FROM principals WHERE session_id='sess-no-scopes'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a credential exists for the refused launch: %d %v", n, err)
	}
	// The same launcher with operate+maintain+read boots with the defaults.
	spec, err := r.launchAs(principalCtx("device", "maintain", "operate", "read"), "sess-tachyon", "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.MCPScopes, launchprofile.DefaultMCPScopes) {
		t.Fatalf("spec scopes %v, want %v", spec.MCPScopes, launchprofile.DefaultMCPScopes)
	}
	if got := r.storedScopes("sess-tachyon"); !slices.Equal(got, launchprofile.DefaultMCPScopes) {
		t.Fatalf("credential scopes %v, want %v", got, launchprofile.DefaultMCPScopes)
	}
}

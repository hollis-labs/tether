package config

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/go-apppaths/paths"
)

// hermeticPaths pins HOME and all four $XDG_*_HOME roots into per-test temp
// dirs so layout resolution (and the go-apppaths fallback) lands entirely
// under t.TempDir rather than the developer's real home. Once the catalog can
// fall back to ~/.local/share/tether/... an unisolated test would otherwise
// touch — and on the daemon path, materialize — the developer's real XDG
// roots. Per the cutover runbook's sharp edge #5, pin the XDG vars (and HOME
// for completeness) into the temp dir.
func hermeticPaths(t *testing.T) (data, state, cache, config string) {
	t.Helper()
	home := t.TempDir()
	data = filepath.Join(home, "xdg-data")
	state = filepath.Join(home, "xdg-state")
	cache = filepath.Join(home, "xdg-cache")
	config = filepath.Join(home, "xdg-config")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_CONFIG_HOME", config)
	return data, state, cache, config
}

// TestResolveLayout_XDGRoots confirms ResolveLayout resolves the four XDG
// roots and the main DB under the hermetic temp home for appName "tether".
func TestResolveLayout_XDGRoots(t *testing.T) {
	data, state, cache, cfg := hermeticPaths(t)

	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	if got, want := layout.DataDir(), filepath.Join(data, "tether"); got != want {
		t.Errorf("DataDir = %q, want %q", got, want)
	}
	if got, want := layout.StateDir(), filepath.Join(state, "tether"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
	if got, want := layout.CacheDir(), filepath.Join(cache, "tether"); got != want {
		t.Errorf("CacheDir = %q, want %q", got, want)
	}
	if got, want := layout.ConfigDir(), filepath.Join(cfg, "tether"); got != want {
		t.Errorf("ConfigDir = %q, want %q", got, want)
	}
	if got, want := layout.MainDB(), filepath.Join(data, "tether", "workspaces", "default", "main.db"); got != want {
		t.Errorf("MainDB = %q, want %q", got, want)
	}
}

// TestResolveStateDB_FallbackWhenCatalogOmitsKey proves that when the catalog
// omits state_db, ResolveStateDB returns the go-apppaths MainDB fallback under
// the hermetic XDG data root.
func TestResolveStateDB_FallbackWhenCatalogOmitsKey(t *testing.T) {
	data, _, _, _ := hermeticPaths(t)

	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	got := ResolveStateDB(Defaults{ /* StateDB omitted */ }, layout)
	want := filepath.Join(data, "tether", "workspaces", "default", "main.db")
	if got != want {
		t.Errorf("ResolveStateDB (catalog omits state_db) = %q, want fallback %q", got, want)
	}
}

// TestResolveStateDB_ExplicitCatalogValueWins is the precedence guard demanded
// by the migration plan and cutover runbook sharp edge #1: an explicit catalog
// state_db MUST keep winning over the go-apppaths fallback — even when
// TETHER_DB_PATH is set and an XDG data root exists. The catalog value is NOT
// routed through paths.WithDBOverride, so TETHER_DB_PATH cannot outrank it.
func TestResolveStateDB_ExplicitCatalogValueWins(t *testing.T) {
	hermeticPaths(t)

	// A TETHER_DB_PATH env override is present — go-apppaths honors it
	// natively for layout.MainDB(). It must NOT shadow the explicit catalog
	// value, because ResolveStateDB returns the catalog value directly.
	envOverride := filepath.Join(t.TempDir(), "from-env.db")
	t.Setenv("TETHER_DB_PATH", envOverride)

	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	catalogValue := filepath.Join(t.TempDir(), "explicit", "tether.db")
	got := ResolveStateDB(Defaults{StateDB: catalogValue}, layout)
	if got != Expand(catalogValue) {
		t.Errorf("ResolveStateDB with explicit catalog state_db = %q, want catalog value %q", got, Expand(catalogValue))
	}
	if got == envOverride {
		t.Errorf("ResolveStateDB returned the TETHER_DB_PATH override %q — the catalog value must win", envOverride)
	}
	if got == layout.MainDB() {
		t.Errorf("ResolveStateDB returned the go-apppaths fallback %q — the catalog value must win", layout.MainDB())
	}
}

// TestResolveWorkspaceRoot_PrecedenceAndFallback covers both branches:
// explicit catalog workspace_root wins; otherwise the fallback is a
// "workspaces" dir under the go-apppaths data root.
func TestResolveWorkspaceRoot_PrecedenceAndFallback(t *testing.T) {
	data, _, _, _ := hermeticPaths(t)

	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	fallback := ResolveWorkspaceRoot(Defaults{}, layout)
	if want := filepath.Join(data, "tether", "workspaces"); fallback != want {
		t.Errorf("ResolveWorkspaceRoot fallback = %q, want %q", fallback, want)
	}

	explicit := filepath.Join(t.TempDir(), "ws")
	if got := ResolveWorkspaceRoot(Defaults{WorkspaceRoot: explicit}, layout); got != Expand(explicit) {
		t.Errorf("ResolveWorkspaceRoot explicit = %q, want %q", got, Expand(explicit))
	}
}

// TestResolveTempRoot_PrecedenceAndFallback covers both branches: explicit
// catalog temp_root wins; otherwise the fallback is the go-apppaths cache root.
func TestResolveTempRoot_PrecedenceAndFallback(t *testing.T) {
	_, _, cache, _ := hermeticPaths(t)

	layout, err := ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		t.Fatalf("ResolveLayout: %v", err)
	}

	fallback := ResolveTempRoot(Defaults{}, layout)
	if want := filepath.Join(cache, "tether"); fallback != want {
		t.Errorf("ResolveTempRoot fallback = %q, want %q", fallback, want)
	}

	explicit := filepath.Join(t.TempDir(), "tmp")
	if got := ResolveTempRoot(Defaults{TempRoot: explicit}, layout); got != Expand(explicit) {
		t.Errorf("ResolveTempRoot explicit = %q, want %q", got, Expand(explicit))
	}
}

// TestLoad_PopulatesPaths confirms Load attaches a resolved go-apppaths Layout
// to Catalog.Paths so downstream callsites can reach the fallback.
func TestLoad_PopulatesPaths(t *testing.T) {
	data, _, _, _ := hermeticPaths(t)

	cat, err := Load(filepath.Join("..", "..", "examples", "catalog"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cat.Paths.App() != appName {
		t.Errorf("Catalog.Paths.App() = %q, want %q", cat.Paths.App(), appName)
	}
	if want := filepath.Join(data, "tether", "workspaces", "default", "main.db"); cat.Paths.MainDB() != want {
		t.Errorf("Catalog.Paths.MainDB() = %q, want %q", cat.Paths.MainDB(), want)
	}
}

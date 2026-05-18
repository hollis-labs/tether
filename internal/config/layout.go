package config

import (
	"path/filepath"

	"github.com/hollis-labs/go-apppaths/paths"
)

// appName is Tether's go-apppaths application identity. It drives the XDG
// roots (~/.local/share/tether, ~/.local/state/tether, ~/.cache/tether,
// ~/.config/tether) and the TETHER_* env-var prefix go-apppaths reads
// natively (TETHER_DB_PATH, TETHER_WORKSPACE).
//
// The binary is `mux`, but the app identity is `tether` — matching the
// catalog dir ~/.tether/ and the module path. Naming the app `mux` would
// diverge the XDG roots and env prefix from the rest of Tether's surface.
const appName = "tether"

// ResolveLayout resolves Tether's on-disk layout via go-apppaths in the
// default (XDG) mode.
//
// No WithLegacyNames: Tether's pre-rename DB at ~/tether/state/agent-mux.db
// is a hand-made path, not an XDG root, so the adoption migration (which only
// moves ~/.local/share/<legacy> -> ~/.local/share/<app>) cannot reach it.
//
// No WithProjectMode: the CWD-local layout is deliberately not used.
//
// The resolved Layout supplies the FALLBACK storage paths for the case where
// the catalog's global.yaml omits state_db / workspace_root / temp_root. The
// explicit catalog values, when set, still win — see ResolveStateDB,
// ResolveWorkspaceRoot, and ResolveTempRoot. Callers that only introspect
// (the `mux path` subcommand) pass paths.WithoutMaterialize().
func ResolveLayout(extra ...paths.Option) (paths.Layout, error) {
	return paths.Resolve(appName, extra...)
}

// ResolveStateDB returns the effective state DB path: the explicit catalog
// state_db when set (Expand'd), else the go-apppaths MainDB fallback.
//
// PRECEDENCE IS EXPLICIT AND DELIBERATE. The catalog value is NOT routed
// through paths.WithDBOverride — doing so would let TETHER_DB_PATH silently
// outrank an explicit catalog state_db. The catalog value always wins here;
// the lib only fills the gap when the catalog omits the key.
func ResolveStateDB(d Defaults, layout paths.Layout) string {
	if d.StateDB != "" {
		return Expand(d.StateDB)
	}
	return layout.MainDB()
}

// ResolveWorkspaceRoot returns the effective workspace root: the explicit
// catalog workspace_root when set (Expand'd), else a "workspaces" directory
// under the go-apppaths data root. Per-session logs/sessions/prompts/state
// dirs are subdirectories of the per-session workspace dir, so they inherit
// this fallback transitively — no separate key.
func ResolveWorkspaceRoot(d Defaults, layout paths.Layout) string {
	if d.WorkspaceRoot != "" {
		return Expand(d.WorkspaceRoot)
	}
	return filepath.Join(layout.DataDir(), "workspaces")
}

// ResolveTempRoot returns the effective temp root: the explicit catalog
// temp_root when set (Expand'd), else the go-apppaths cache root. Unlike
// state_db and workspace_root, temp_root callers tolerate an empty value
// today; with the fallback it is always populated.
func ResolveTempRoot(d Defaults, layout paths.Layout) string {
	if d.TempRoot != "" {
		return Expand(d.TempRoot)
	}
	return layout.CacheDir()
}

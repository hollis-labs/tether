package app

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
)

// linkCodexHostAuth points a codex launch's planted auth.json at the host's
// codex login.
//
// INTERIM (CW-20261001-0031): CW-20260930-0106 (native launches on the wrapper)
// replaces this. A codex session runs with CODEX_HOME set to its boot dir, and
// codex reads credentials only from $CODEX_HOME/auth.json. go-providers plants
// that file as an empty placeholder (EffectCodexAuthJSON: credentials are for
// explicit runtime preparation), and agentkit's providerplant pins
// LegacyAllowHostEffects off, so the ambient copy Nanite opts into
// (nanite#349, CW-20261001-0021) is not reachable from here. Without this every
// model call 401s.
//
// A symlink, not a copy: codex rewrites auth.json in place when it refreshes
// its tokens, so a refresh inside a session lands in the host's file instead of
// rotating the host's refresh token out from under it (the snapshot hazard in
// CW-20261001-0027), and no credential copies collect in boot dirs. Tether
// never reads the file. The link's target is outside the workspace, so a
// session under a read-restricted sandbox profile could not follow it; no
// catalog agent sets one today.
//
// A host that is not logged in keeps the empty placeholder and still launches.
func linkCodexHostAuth(providerBrand string, prepared *agentlaunch.PreparedLaunch) {
	if providerBrand != "codex" || prepared == nil {
		return
	}
	codexHome := prepared.Env["CODEX_HOME"]
	if codexHome == "" {
		return // codex will read the host's own CODEX_HOME directly
	}
	hostAuth := codexHostAuthPath()
	if hostAuth == "" {
		return
	}
	linked, err := symlinkAuthJSON(filepath.Join(codexHome, "auth.json"), hostAuth)
	if err != nil {
		log.Printf("codex auth: link %s to the host login failed, session keeps the empty auth.json: %v", codexHome, err)
		return
	}
	if !linked {
		log.Printf("codex auth: no host login at %s; session keeps the empty auth.json", hostAuth)
	}
}

// codexHostAuthPath is where the host's codex login lives: $CODEX_HOME of the
// daemon, else ~/.codex/auth.json — go-providers' own lookup.
func codexHostAuthPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		abs, err := filepath.Abs(filepath.Join(h, "auth.json"))
		if err != nil {
			return ""
		}
		return abs
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// symlinkAuthJSON replaces planted with a symlink to hostAuth. It reports
// false, leaving planted alone, when there is no host file to point at.
func symlinkAuthJSON(planted, hostAuth string) (bool, error) {
	if _, err := os.Stat(hostAuth); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if filepath.Clean(planted) == filepath.Clean(hostAuth) {
		return false, nil
	}
	if err := os.Remove(planted); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.Symlink(hostAuth, planted); err != nil {
		return false, err
	}
	return true, nil
}

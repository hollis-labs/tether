package app

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/hollis-labs/libs/harness/core/sandbox"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/teamactivation"
	"gopkg.in/yaml.v3"
)

// Installed bytes are a separate observation from the loaded policy. The same
// bytes are parsed and hashed once; no path or raw text enters the response.
// A missing/ambiguous/oversized source stays unknown rather than being guessed
// from the profile ID (catalog filenames need not equal their declared IDs).
func (s *Service) inspectInstalledTeamSandbox(p *launch.Plan, e *teamactivation.Launch) {
	if e.Sandbox == "" || s.CatalogRoot == "" {
		return
	}
	dir := filepath.Join(s.CatalogRoot, "sandbox-profiles")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 1024 {
		return
	}
	found := false
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 1<<20 {
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // fixed catalog directory, entry name from ReadDir
		if err != nil {
			return
		}
		var sp sandbox.Profile
		if yaml.Unmarshal(raw, &sp) != nil || sp.ID != e.Sandbox {
			continue
		}
		if found {
			e.InstalledSandboxFileDigest = ""
			e.InstalledSandboxPolicyDigest = ""
			return
		}
		found = true
		sum := sha256.Sum256(raw)
		e.InstalledSandboxFileDigest = "sha256:" + hex.EncodeToString(sum[:])
		if installed, err := s.teamPlanEvidence(p, e.Worker, &sp); err == nil {
			e.InstalledSandboxPolicyDigest = installed.EffectiveSandboxPolicyDigest
		}
	}
}

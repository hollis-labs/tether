package launchartifacts_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"
	"github.com/hollis-labs/substrate/harness/workspace"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"
)

// An accepted artifact operation never becomes credential authorization. The
// published projection's auth.json placeholder therefore still refuses before
// artifacts; a future explicit credential mapping must resolve this boundary.
func TestArtifactCustody_CodexCredentialSlotRemainsReserved(t *testing.T) {
	compiled := compileLaunch(t, "codex")
	prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, testfixture.Admission(t, func() any { return compiled }))
	if err != nil {
		t.Fatal(err)
	}
	defer custody.Close()
	err = providerplant.Plant(context.Background(), prepared, providerplant.WithArtifactAuthorization(custody.Authorize))
	var refusal *workspace.Refusal
	if !errors.As(err, &refusal) || refusal.Code != workspace.CodeReservedArtifactPath {
		t.Fatalf("credential slot: got %v; want reserved artifact path", err)
	}
	for _, name := range []string{"auth.json", "AGENTS.md", "config.toml", ".materialize/manifest.json"} {
		if _, err := os.Lstat(filepath.Join(prepared.PlantedBootDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential refusal changed boot artifact %s: %v", name, err)
		}
	}
}

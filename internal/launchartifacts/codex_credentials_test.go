package launchartifacts_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/substrate/harness/workspace"
	"github.com/hollis-labs/substrate/harness/workspace/effects"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/launchartifacts/testfixture"
)

func ownedCodexSource(t *testing.T) (string, *launchartifacts.CodexHome) {
	t.Helper()
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "auth.json"), []byte(`{"fixture":"original"}`), 0600); err != nil {
		t.Fatal(err)
	}
	home, err := launchartifacts.CaptureCodexHome(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = home.Close() })
	return path, home
}

func TestCodexCredentials_LinkRefreshAndDurableEvidence(t *testing.T) {
	source, home := ownedCodexSource(t)
	compiled := compileLaunch(t, "codex")
	if err := os.Chmod(compiled.Plan.Workspace.TempPrefix, 0700); err != nil {
		t.Fatal(err)
	}
	admission := testfixture.Admission(t, func() any { return compiled })
	prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, admission)
	if err != nil {
		t.Fatal(err)
	}
	defer custody.Close()
	// A later environment change cannot retarget the already captured source.
	t.Setenv("CODEX_HOME", t.TempDir())
	if err := custody.PlantCodex(context.Background(), prepared, home); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(prepared.PlantedBootDir, "auth.json")
	if target, err := os.Readlink(planted); err != nil || target != filepath.Join(source, "auth.json") {
		t.Fatalf("captured source link: %v", err)
	}
	// Refresh writes go through the link to the owned source, never a snapshot.
	if err := os.WriteFile(planted, []byte(`{"fixture":"refreshed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(source, "auth.json")) //nolint:gosec // literal test-owned stand-in
	if err != nil || string(contents) != `{"fixture":"refreshed"}` {
		t.Fatalf("source refresh failed: %v", err)
	}
	// Atomic reauthentication preserves the logical source path.
	newAuth := filepath.Join(source, "auth.new")
	if err := os.WriteFile(newAuth, []byte(`{"fixture":"reauthenticated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newAuth, filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	contents, err = os.ReadFile(planted) //nolint:gosec // literal test-owned stand-in
	if err != nil || string(contents) != `{"fixture":"reauthenticated"}` {
		t.Fatalf("logical source path lost: %v", err)
	}
	controls, err := os.ReadDir(admission.ControlParent)
	if err != nil {
		t.Fatal(err)
	}
	var receipt workspace.Receipt
	for _, control := range controls {
		paths, err := filepath.Glob(filepath.Join(admission.ControlParent, control.Name(), "receipt-*.json"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path) //nolint:gosec // owned control receipt, contains no credential bytes
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if receipt.OperationID != admission.OperationID || receipt.Phase != workspace.ArtifactsCommitted {
		t.Fatal("missing same-operation committed receipt")
	}
	var intent, complete bool
	for _, evidence := range receipt.EffectEvidence {
		if evidence.Kind != effects.CredentialLinks || evidence.Header.InputDigest != receipt.InputDigest {
			t.Fatal("credential evidence escaped artifact operation")
		}
		intent = intent || evidence.Phase == effects.LinkIntentPhase
		complete = complete || evidence.Phase == effects.CompletePhase && evidence.Outcome == effects.Applied
	}
	if !intent || !complete {
		t.Fatal("credential link lacks durable intent/completion evidence")
	}
	manifest, err := workspace.InspectRoot(receipt.Roots[0].Root)
	if err != nil || manifest.Manifest == nil {
		t.Fatalf("managed manifest: %v", err)
	}
	for _, entry := range manifest.Manifest.Entries {
		if entry.Path == "auth.json" {
			t.Fatal("credential link entered the managed artifact manifest")
		}
	}
}

func TestCodexCredentials_RefusesChangedSourceOrCustody(t *testing.T) {
	for _, change := range []string{"source-absent", "source-directory", "source-symlink", "home-replaced", "candidate-replaced", "closed-home", "auth-overlay", "other-credential"} {
		t.Run(change, func(t *testing.T) {
			source, home := ownedCodexSource(t)
			compiled := compileLaunch(t, "codex")
			if err := os.Chmod(compiled.Plan.Workspace.TempPrefix, 0700); err != nil {
				t.Fatal(err)
			}
			if change == "auth-overlay" {
				compiled.Plan.Injection.BootDirOverlay = map[string]string{"auth.json": ""}
			}
			if change == "other-credential" {
				compiled.Plan.Injection.BootDirOverlay = map[string]string{".credentials.json": "unapproved"}
			}
			prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, testfixture.Admission(t, func() any { return compiled }))
			if err != nil {
				t.Fatal(err)
			}
			defer custody.Close()
			auth := filepath.Join(source, "auth.json")
			switch change {
			case "source-absent", "source-directory", "source-symlink":
				if err := os.Remove(auth); err != nil {
					t.Fatal(err)
				}
				if change == "source-directory" {
					if err := os.Mkdir(auth, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if change == "source-symlink" {
					if err := os.Symlink(filepath.Join(t.TempDir(), "unapproved"), auth); err != nil {
						t.Fatal(err)
					}
				}
			case "home-replaced":
				if err := os.Rename(source, source+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(auth, []byte(`{"fixture":"replacement"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "candidate-replaced":
				if err := os.Rename(prepared.PlantedBootDir, prepared.PlantedBootDir+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(prepared.PlantedBootDir, 0700); err != nil {
					t.Fatal(err)
				}
			case "closed-home":
				if err := home.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := custody.PlantCodex(context.Background(), prepared, home); err == nil {
				t.Fatal("changed/unapproved credential mapping accepted")
			}
			for _, path := range []string{"auth.json", "config.toml", ".materialize/manifest.json"} {
				if _, err := os.Lstat(filepath.Join(prepared.PlantedBootDir, path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refusal mutated %s: %v", path, err)
				}
			}
		})
	}
}

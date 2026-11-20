package gatewaylaunch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/sandbox"
)

func TestNarrowedSandboxMountsSessionCredentialDirectoryAfterPrivateDev(t *testing.T) {
	path, stop, err := Start(func(context.Context) (Credential, error) {
		return Credential{Token: "synthetic", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	original := sandbox.Profile{ID: "narrowed", Net: true, Subprocess: true, FS: sandbox.FSSpec{Read: []string{"/etc"}}}
	updated, err := ReadableProfile(original, path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(original.FS.Read) != 1 {
		t.Fatal("catalog profile mutated")
	}
	args, err := sandbox.BuildBwrapArgs(updated, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(path)
	dev, credential := -1, -1
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--dev" && args[i+1] == "/dev" {
			dev = i
		}
		if args[i] == "--ro-bind-try" && args[i+1] == directory && args[i+2] == directory {
			credential = i
		}
		if (args[i] == "--bind" || args[i] == "--ro-bind-try") && args[i+1] == path {
			t.Fatal("file bind hides future atomic refresh")
		}
	}
	if dev < 0 || credential <= dev {
		t.Fatal("private credential directory not visible after /dev replacement")
	}
	if slices.Contains(updated.FS.Read, "/dev/shm") {
		t.Fatal("exposed other sessions' tmpfs directories")
	}
	for _, profile := range []sandbox.Profile{{}, {ID: "host", HostFilesystem: true}} {
		updated, err := ReadableProfile(profile, path, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if len(updated.FS.Read) != 0 {
			t.Fatal("unnecessary grant on host-filesystem profile")
		}
	}
}

func TestSandboxRejectsHiddenGatewayCredentials(t *testing.T) {
	path, stop, err := Start(func(context.Context) (Credential, error) {
		return Credential{Token: "synthetic", ExpiresAt: time.Now().Add(time.Minute)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Dir(path), alias); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"/", "/dev", "/dev/shm", filepath.Dir(path), path, RunTokenPath(path), alias, filepath.Join(alias, "run.token")} {
		for _, host := range []bool{true, false} {
			for _, deny := range []bool{true, false} {
				p := sandbox.Profile{ID: "hidden", HostFilesystem: host}
				if deny {
					p.FS.Deny = []string{hidden}
				} else {
					p.FS.Hide = []string{hidden}
				}
				if _, err := ReadableProfile(p, path, t.TempDir()); err == nil {
					t.Errorf("accepted hidden path %s", hidden)
				}
			}
		}
	}
	if err := CheckHidden([]string{"/other"}, path, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []sandbox.ConfinementMode{"", sandbox.ConfinementRequired} {
		if CheckResolvedPolicy(&sandbox.ResolvedAccessPolicy{Mode: mode}) == nil {
			t.Fatal("accepted incompatible resolved confinement")
		}
	}
	if CheckResolvedPolicy(nil) != nil || CheckResolvedPolicy(&sandbox.ResolvedAccessPolicy{Mode: sandbox.ConfinementDisabled}) != nil {
		t.Fatal("rejected unconfined policy")
	}
}

package gatewaylaunch

import (
	"context"
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
	updated := ReadableProfile(original, path)
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
		if len(ReadableProfile(profile, path).FS.Read) != 0 {
			t.Fatal("unnecessary grant on host-filesystem profile")
		}
	}
}

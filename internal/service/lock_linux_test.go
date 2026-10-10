//go:build linux

package service

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLockOwnerHelper(t *testing.T) {
	if os.Getenv("TETHER_SERVICE_OWNER_HELPER") != "1" {
		return
	}
	owner, err := processIdentity(os.Getpid())
	if err != nil {
		os.Exit(2)
	}
	owner.Token = "11111111111111111111111111111111"
	data, err := json.Marshal(owner)
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(os.Getenv("TETHER_SERVICE_OWNER_FILE"), data, 0600); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestInstallLockLiveDeadAndReusedIdentity(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lock")
		unlock, err := acquireLock(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		if release, err := acquireLock(ctx, path); err == nil {
			release()
			t.Fatal("live lock reclaimed")
		}
		if _, err := readLockOwner(path); err != nil {
			t.Fatal("live owner's record removed", err)
		}
	})
	t.Run("dead", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "lock")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestLockOwnerHelper$")
		cmd.Env = []string{"HOME=" + dir, "TMPDIR=" + dir, "TETHER_SERVICE_OWNER_HELPER=1", "TETHER_SERVICE_OWNER_FILE=" + filepath.Join(path, "pid")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("owned helper: %v %s", err, out)
		}
		old, err := readLockOwner(path)
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := acquireLock(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		current, err := readLockOwner(path)
		if err != nil || current.Token == old.Token {
			t.Fatal("dead incarnation not reclaimed", err)
		}
	})
	t.Run("reused-pid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "lock")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		old, err := processIdentity(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		old.Start = "0"
		old.Token = "11111111111111111111111111111111"
		data, _ := json.Marshal(old)
		if err := os.WriteFile(filepath.Join(path, "pid"), data, 0600); err != nil {
			t.Fatal(err)
		}
		unlock, err := acquireLock(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
	})
}

func TestLockUnknownIdentityRefusedAndGuardCancelable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "pid"), []byte(`{"pid":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(context.Background(), path); err == nil {
		t.Fatal("PID-only ownership reclaimed")
	}
	guard, err := lockGuard(context.Background(), path+".guard")
	if err != nil {
		t.Fatal(err)
	}
	defer guard()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if release, err := lockGuard(ctx, path+".guard"); err == nil {
		release()
		t.Fatal("kernel guard ignored deadline")
	}
}

func TestLockReleaseKeepsReplacementOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := readLockOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Token = "22222222222222222222222222222222"
	data, _ := json.Marshal(replacement)
	if err := writeAtomic(filepath.Join(path, "pid"), data); err != nil {
		t.Fatal(err)
	}
	unlock()
	if current, err := readLockOwner(path); err != nil || current.Token != replacement.Token {
		t.Fatal("release removed another ownership token", err)
	}
}

func TestLockReleaseKeepsReplacementDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := acquireLock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := readLockOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(owner)
	if err := writeAtomic(filepath.Join(path, "pid"), data); err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("release removed replacement directory", err)
	}
}

func TestLockMissingAndCorruptIdentityStayUnknown(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lock")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "corrupt" {
				owner, err := processIdentity(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				owner.Boot = "corrupt"
				owner.Token = "11111111111111111111111111111111"
				data, _ := json.Marshal(owner)
				if err := writeAtomic(filepath.Join(path, "pid"), data); err != nil {
					t.Fatal(err)
				}
			}
			if release, err := acquireLock(context.Background(), path); err == nil {
				release()
				t.Fatal("unverified owner reclaimed")
			}
		})
	}
}

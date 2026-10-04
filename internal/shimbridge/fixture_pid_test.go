//go:build linux

package shimbridge

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestFixtureChildPIDRejectsIncompleteWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.pid")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	// Force the create-before-write ordering from the fake child's old writer.
	if pid, err := fixtureChildPID(path); err == nil {
		t.Fatalf("incomplete witness accepted as PID %d", pid)
	}
	if _, err = f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if pid, err := fixtureChildPID(path); err != nil || pid != os.Getpid() {
		t.Fatalf("completed witness: PID %d, error %v", pid, err)
	}
}

func TestFixtureChildPIDRejectsMalformedWitness(t *testing.T) {
	for _, text := range []string{"not-a-pid", "0", "-1"} {
		t.Run(text, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "child.pid")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if pid, err := fixtureChildPID(path); err == nil {
				t.Fatalf("invalid witness accepted as PID %d", pid)
			}
		})
	}
}

func TestFixtureChildPIDAtomicPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.pid")
	if err := publishFixtureChildPID(path, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if pid, err := fixtureChildPID(path); err != nil || pid != os.Getpid() {
		t.Fatalf("published witness: PID %d, error %v", pid, err)
	}
}

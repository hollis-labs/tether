package identity_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func rotateFixture(t *testing.T) (*identity.Store, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	s, _ := identityStore(t)
	path := filepath.Join(t.TempDir(), "run", "operator.token")
	if err := s.EnsureOperator(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	old, err := identity.ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path, old
}

func TestRotateOperatorRevokesOldAndInstallsNew(t *testing.T) {
	s, path, old := rotateFixture(t)
	ctx := context.Background()
	res, err := s.RotateOperator(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OldFileValid || res.Revoked != 1 || res.OldHashPrefix == "" || res.NewHashPrefix == "" || res.OldHashPrefix == res.NewHashPrefix {
		t.Fatalf("result %+v", res)
	}
	if _, err := s.Verify(ctx, old); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("old operator token still verifies after rotation")
	}
	fresh, err := identity.ReadTokenFile(path) // also enforces 0600, regular, owner
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("token file was not replaced")
	}
	p, err := s.Verify(ctx, fresh)
	if err != nil || p.ID != identity.OperatorID || p.Kind != "operator" || !p.Verified {
		t.Fatalf("new token does not verify as the operator: %v %+v", err, p)
	}
	if strings.HasPrefix(res.NewHashPrefix, "tth_") || strings.Contains(res.NewHashPrefix, fresh) {
		t.Fatal("result leaks credential material")
	}
	// the daemon-start check accepts the rotated pair
	if err := s.EnsureOperator(ctx, path); err != nil {
		t.Fatalf("EnsureOperator rejects the rotated pair: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "operator.token" {
			t.Fatalf("leftover file %s", e.Name())
		}
	}
}

func TestRotateOperatorFailureKeepsOldPairValid(t *testing.T) {
	s, path, old := rotateFixture(t)
	ctx := context.Background()
	identity.SetRotateAfterReplace(func() error { return errors.New("injected failure after file replace") })
	t.Cleanup(func() { identity.SetRotateAfterReplace(nil) })
	if _, err := s.RotateOperator(ctx, path); err == nil {
		t.Fatal("injected failure not reported")
	}
	if p, err := s.Verify(ctx, old); err != nil || p.ID != identity.OperatorID {
		t.Fatalf("old token no longer valid after failed rotation: %v", err)
	}
	got, err := identity.ReadTokenFile(path)
	if err != nil || got != old {
		t.Fatalf("token file not restored to the old credential: %v", err)
	}
	if err := s.EnsureOperator(ctx, path); err != nil {
		t.Fatalf("old pair no longer consistent: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != "operator.token" {
			t.Fatalf("leftover file %s after failed rotation", e.Name())
		}
	}
	// and a retry without the failure succeeds
	identity.SetRotateAfterReplace(nil)
	if _, err := s.RotateOperator(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func TestRotateOperatorRecoversMissingOrBrokenTokenFile(t *testing.T) {
	for name, damage := range map[string]func(string){
		"missing": func(p string) { _ = os.Remove(p) },
		"loose":   func(p string) { _ = os.Chmod(p, 0o644) },
		"garbage": func(p string) { _ = os.WriteFile(p, []byte("nope\n"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			s, path, old := rotateFixture(t)
			ctx := context.Background()
			damage(path)
			if err := s.EnsureOperator(ctx, path); err == nil {
				t.Fatal("fixture did not break the pair")
			}
			res, err := s.RotateOperator(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if res.OldFileValid {
				t.Fatal("damaged file reported valid")
			}
			if _, err := s.Verify(ctx, old); err == nil {
				t.Fatal("old credential survived recovery")
			}
			if err := s.EnsureOperator(ctx, path); err != nil {
				t.Fatalf("recovered pair rejected: %v", err)
			}
		})
	}
}

func TestRotateOperatorRefusesNonRegularTokenPath(t *testing.T) {
	s, path, old := rotateFixture(t)
	target := filepath.Join(filepath.Dir(path), "elsewhere")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RotateOperator(context.Background(), path); err == nil {
		t.Fatal("rotated through a symlink")
	}
	if _, err := s.Verify(context.Background(), old); err != nil {
		t.Fatal("old credential revoked by a refused rotation")
	}
}

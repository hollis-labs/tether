package identity_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func identityStore(t *testing.T) (*identity.Store, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return identity.NewStore(db.DB()), db
}

func TestIdentityMintVerifyAndRevoke(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, db := identityStore(t)
	ctx := context.Background()
	if err := db.CreateSession(store.SessionRow{ID: "test", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	want := identity.Principal{ID: "msg://session/local/test", Kind: "session", SessionID: "test", Display: "Test", Scopes: []string{"session.write"}, Addresses: []string{"msg://session/local/test"}, CreatedBy: identity.OperatorID}
	token, err := s.Mint(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "tth_") || len(token) != 47 {
		t.Fatalf("invalid token shape")
	}
	other, err := identity.NewToken()
	if err != nil || token == other {
		t.Fatal("token entropy/mint failed")
	}
	var hash string
	if err := db.DB().QueryRow(`SELECT token_hash FROM principals WHERE principal_id = ?`, want.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != identity.HashToken(token) || strings.Contains(hash, token) {
		t.Fatal("persisted raw token or wrong hash")
	}
	got, err := s.Verify(ctx, token)
	if err != nil || got.ID != want.ID || got.CreatedBy != want.CreatedBy || got.SessionID != want.SessionID || got.Scopes[0] != want.Scopes[0] {
		t.Fatalf("verify principal: %+v, %v", got, err)
	}
	for _, bad := range []string{"", "tth_bad", other, token + "extra"} {
		if _, err := s.Verify(ctx, bad); !errors.Is(err, identity.ErrInvalidToken) {
			t.Fatalf("bad credential accepted: %v", err)
		}
	}
	parent := identity.WithPrincipal(ctx, got)
	got.Scopes[0] = "*"
	carried, ok := identity.FromContext(parent)
	if !ok || carried.Scopes[0] != "session.write" {
		t.Fatal("context principal aliases caller slices")
	}
	if err := s.RevokeSession(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatalf("revoked credential accepted: %v", err)
	}
	if err := s.RevokeSession(ctx, "test"); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityExpiryAndOperatorBootstrap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, db := identityStore(t)
	ctx := context.Background()
	at := time.Now().Add(time.Hour)
	token, err := s.Mint(ctx, identity.Principal{ID: "svc:test", Kind: "service", ExpiresAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`UPDATE principals SET expires_at = ? WHERE principal_id = 'svc:test'`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("expired token accepted")
	}
	path := filepath.Join(t.TempDir(), "run", "operator.token")
	if err := s.EnsureOperator(ctx, path); err != nil {
		t.Fatal(err)
	}
	first, err := identity.ReadTokenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureOperator(ctx, path); err != nil {
		t.Fatal(err)
	}
	second, err := identity.ReadTokenFile(path)
	if err != nil || first != second {
		t.Fatal("restart rotated operator credential")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureOperator(ctx, path); err == nil {
		t.Fatal("loose operator file repaired or accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, identity.OperatorID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureOperator(ctx, path); err == nil {
		t.Fatal("revoked operator resurrected")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureOperator(ctx, path); err == nil {
		t.Fatal("missing file rotated existing principal")
	}
}

func TestIdentityTokenFileFailsClosed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "good.token")
	if err := identity.WriteTokenFile(path, token); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadTokenFile(path); err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(path, token); err == nil {
		t.Fatal("existing file overwritten")
	}
	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadTokenFile(link); err == nil {
		t.Fatal("symlink accepted")
	}
	fifo := filepath.Join(dir, "fifo.token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadTokenFile(fifo); err == nil {
		t.Fatal("fifo accepted")
	}
	if _, err := identity.ReadTokenFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadTokenFile(path); err == nil {
		t.Fatal("oversized content accepted")
	}
}

func TestIdentityTokenFileRejectsValidPrefixWithUnreadSuffix(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "token")
	// The first bounded read looks valid after trimming; the remainder must
	// still make the file invalid rather than being silently ignored.
	body := token + strings.Repeat(" ", 256-len(token)) + "unread-junk"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.ReadTokenFile(path); err == nil {
		t.Fatal("unread suffix accepted")
	}
}

func TestIdentityMultipleTokensForOnePrincipal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := identityStore(t)
	ctx := context.Background()
	p := identity.Principal{ID: "svc:rotate", Kind: "service"}
	first, err := s.Mint(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Mint(ctx, p)
	if err != nil || first == second {
		t.Fatal("second mint failed", err)
	}
	for _, token := range []string{first, second} {
		verified, err := s.Verify(ctx, token)
		if err != nil || verified.ID != p.ID {
			t.Fatal("wrong stable principal", err)
		}
	}
	if err := s.Revoke(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first, second} {
		if _, err := s.Verify(ctx, token); !errors.Is(err, identity.ErrInvalidToken) {
			t.Fatal("principal revocation did not revoke every token")
		}
	}
}

func TestIdentityRevokeTokenPreservesOtherCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := identityStore(t)
	ctx := context.Background()
	p := identity.Principal{ID: "svc:replacement", Kind: "service"}
	first, err := s.Mint(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Mint(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, first); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("revoked token accepted", err)
	}
	if _, err := s.Verify(ctx, second); err != nil {
		t.Fatal("other token was revoked", err)
	}
}

func TestIdentitySessionReplacementIsAtomicAndCreatedOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ids, db := identityStore(t)
	ctx := context.Background()
	if err := db.CreateSession(store.SessionRow{ID: "retry", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	p := identity.Principal{ID: "msg://session/local/retry", Kind: "session", SessionID: "retry"}
	first, err := ids.MintSessionForLaunch(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER test_mint_failure BEFORE INSERT ON principals BEGIN SELECT RAISE(ABORT,'test mint failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := ids.MintSessionForLaunch(ctx, p); err == nil {
		t.Fatal("replacement unexpectedly succeeded")
	}
	if _, err := ids.Verify(ctx, first); err != nil {
		t.Fatal("failed replacement revoked old credential", err)
	}
	if _, err := db.DB().Exec(`DROP TRIGGER test_mint_failure`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("retry", "running", 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ids.MintSessionForLaunch(ctx, p); err == nil {
		t.Fatal("replaced running session credential")
	}
	if _, err := ids.Verify(ctx, first); err != nil {
		t.Fatal("running credential revoked", err)
	}
}

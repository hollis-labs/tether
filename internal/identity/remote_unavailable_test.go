package identity_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// stallDB holds the store's only connection for d, so every identity lookup
// in that window times out: the database stall of CW-20261114-0168.
func stallDB(t *testing.T, db *store.Store, d time.Duration) {
	t.Helper()
	tx, err := db.DB().Begin()
	if err != nil {
		t.Error(err)
		return
	}
	time.Sleep(d)
	_ = tx.Rollback()
}

// openStream starts a device stream through RemoteMiddleware and returns a
// channel closed when the handler sees its context end.
func openStream(t *testing.T, s *identity.Store, token string) (ended <-chan struct{}, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	started, done, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := identity.RemoteMiddleware(s, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(done)
	}))
	go func() {
		defer close(returned)
		r := httptest.NewRequest("GET", "/environment/events", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not authenticate")
	}
	return done, func() { cancel(); <-returned }
}

// CW-20261114-0168: a device stream (a worker's connection) is not ended because
// its credential could not be re-verified while the database stalled. Before,
// any verification error, a 1s timeout included, read as revocation: workers
// flapped every 10-60s, and the canceled handshakes ordered live sessions killed.
func TestDeviceStreamSurvivesAStalledDatabase(t *testing.T) {
	s, db := identityStore(t)
	expires := time.Now().Add(time.Hour)
	token, err := s.Mint(context.Background(), identity.Principal{ID: "msg://device/w", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}

	ended, stop := openStream(t, s, token)
	stallDB(t, db, 2500*time.Millisecond)
	select {
	case <-ended:
		t.Fatal("a stalled database ended the stream")
	case <-time.After(1500 * time.Millisecond):
	}
	// Revocation still ends it at once.
	if err := s.RevokeToken(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not end the stream")
	}
	stop()
}

// Verification that stays unavailable past the grace still ends the stream.
func TestDeviceStreamEndsWhenVerificationStaysUnavailable(t *testing.T) {
	s, db := identityStore(t)
	expires := time.Now().Add(time.Hour)
	token, err := s.Mint(context.Background(), identity.Principal{ID: "msg://device/w", Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	restore := identity.SetStreamUnavailableGrace(1500 * time.Millisecond)
	defer restore()
	ended, stop := openStream(t, s, token)
	defer stop()
	stalled := make(chan struct{})
	go func() { defer close(stalled); stallDB(t, db, 6*time.Second) }()
	defer func() { <-stalled }()
	select {
	case <-ended:
	case <-time.After(8 * time.Second):
		t.Fatal("a stream whose credential stayed unverifiable past the grace was kept")
	}
}

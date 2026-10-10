package identity_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestRemoteDeviceStreamRevocationUsesCurrentCredential(t *testing.T) {
	s, db := identityStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	expires := time.Now().Add(time.Hour)
	mint := func(id string) string {
		t.Helper()
		token, err := s.Mint(context.Background(), identity.Principal{ID: id, Kind: "device", Scopes: []string{"read"}, ExpiresAt: &expires})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	a, b := mint("msg://device/a"), mint("msg://device/b")
	started := make(chan string, 2)
	finished := make(chan string, 2)
	h := identity.RemoteMiddleware(s, nil, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, _ := identity.FromContext(r.Context())
		started <- p.ID
		<-r.Context().Done()
		finished <- p.ID
	}))
	var wg sync.WaitGroup
	for _, token := range []string{a, b} {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "/environment/events?as=msg://device/forged", nil).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer "+token)
			h.ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("streams did not authenticate")
		}
	}
	if err := s.RevokeToken(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-finished:
		if id != "msg://device/a" {
			t.Fatal("revoked wrong principal")
		}
	case <-time.After(time.Second):
		t.Fatal("same-store revocation did not cancel")
	}
	select {
	case <-finished:
		t.Fatal("another principal was canceled")
	default:
	}
	// A different Store has no in-memory watcher: committed DB revocation is
	// observed by the current-credential polling backstop.
	other := identity.NewStore(db.DB())
	if err := other.RevokeDevice(context.Background(), "msg://device/b"); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-finished:
		if id != "msg://device/b" {
			t.Fatal("wrong cross-store cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("committed revocation not observed")
	}
	wg.Wait()
}

func TestRemoteDeviceExpiryAndNarrowingCancelStreams(t *testing.T) {
	for _, change := range []string{"expiry", "narrow"} {
		t.Run(change, func(t *testing.T) {
			s, db := identityStore(t)
			expires := time.Now().Add(time.Hour)
			token, err := s.Mint(context.Background(), identity.Principal{ID: "msg://device/current", Kind: "device", Scopes: []string{"read", "operate"}, ExpiresAt: &expires})
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			done := make(chan struct{})
			server := httptest.NewServer(identity.RemoteMiddleware(s, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				_ = http.NewResponseController(w).Flush()
				close(started)
				<-r.Context().Done()
			})))
			defer server.Close()
			defer server.CloseClientConnections()
			go func() {
				defer close(done)
				r, _ := http.NewRequest("GET", server.URL+"/environment/events", nil)
				r.Header.Set("Authorization", "Bearer "+token)
				res, err := server.Client().Do(r)
				if err == nil {
					defer func() { _ = res.Body.Close() }()
					_, _ = io.Copy(io.Discard, res.Body)
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("stream not started")
			}
			if change == "expiry" {
				if _, err := db.DB().Exec(`UPDATE principals SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE principal_id='msg://device/current'`); err != nil {
					t.Fatal(err)
				}
			} else {
				p, err := s.Verify(context.Background(), token)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.RenewDevice(context.Background(), p, identity.HashToken(token), []string{"read"}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("revoked/expired stream retained")
			}
		})
	}
}

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestCredentialInventoryAuthorization(t *testing.T) {
	ids, _, operator, router := deviceAuthFixture(t)
	expires := time.Now().Add(time.Hour)
	mint := func(kind, id string, scopes []string) string {
		t.Helper()
		token, err := ids.Mint(context.Background(), identity.Principal{ID: id, Kind: kind, Scopes: scopes, ExpiresAt: &expires})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	admin := mint("device", "msg://device/admin", []string{"admin"})
	reader := mint("device", "msg://device/reader", []string{"read", "maintain"})
	service := mint("service", "msg://service/test", []string{"*"})
	for _, tc := range []struct {
		name, token  string
		remote, unix bool
		mode         identity.Mode
		want         int
	}{
		{"local operator", operator, false, true, identity.Enforce, 200},
		{"remote admin", admin, true, false, identity.Enforce, 200},
		{"remote reader", reader, true, false, identity.Enforce, 403},
		{"remote operator", operator, true, false, identity.Enforce, 401},
		{"local TCP operator", operator, false, false, identity.Enforce, 403},
		{"local service", service, false, true, identity.Enforce, 403},
		{"anonymous observe", "", false, true, identity.Observe, 403},
		{"anonymous off", "", false, true, identity.Off, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/auth/keys", nil)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.unix {
				r = r.WithContext(identity.ConnectionContext(r.Context(), &net.UnixConn{}))
			}
			h := identity.Middleware(tc.mode, ids, nil, router)
			if tc.remote {
				h = identity.RemoteMiddleware(ids, nil, RemoteScopeMiddleware(router))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d", w.Code, tc.want)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("credential inventory is cacheable")
			}
			for _, secret := range []string{operator, admin, reader, service, identity.HashToken(operator), identity.HashToken(admin)} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatal("credential material exposed")
				}
			}
		})
	}
	claimed := identity.WithPrincipal(identity.WithRemoteContext(context.Background()), identity.Principal{ID: "claimed", Kind: "device", Scopes: []string{"admin"}})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/auth/keys", nil).WithContext(claimed))
	if w.Code != 403 {
		t.Fatal("unverified principal accepted")
	}
}

func TestCredentialInventoryPagesCredentialRecordsAndLifecycle(t *testing.T) {
	ids, db, operator, router := deviceAuthFixture(t)
	expires := time.Now().UTC().Add(time.Hour)
	for _, scopes := range [][]string{{"tachyon.approvals_read"}, {"tachyon.approvals_read", "tachyon.approvals_enqueue"}} {
		_, err := ids.Mint(context.Background(), identity.Principal{ID: "producer", Kind: "service", Display: "test producer", Scopes: scopes, ExpiresAt: &expires})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Model historical custody alongside the currently valid credential.
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB().Exec(`UPDATE principals SET expires_at = ?, revoked_at = ?, last_used_at = ? WHERE id = (SELECT min(id) FROM principals WHERE principal_id = 'producer')`, past, past, past); err != nil {
		t.Fatal(err)
	}
	h := identity.Middleware(identity.Enforce, ids, nil, router)
	call := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+operator)
		r = r.WithContext(identity.ConnectionContext(r.Context(), &net.UnixConn{}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var after int64
	seen := map[int64]bool{}
	var sawRevoked, sawCurrent bool
	for {
		w := call("/auth/keys?kind=service&principal=producer&limit=1&after=" + strconv.FormatInt(after, 10))
		if w.Code != 200 {
			t.Fatalf("inventory returned %d", w.Code)
		}
		var page identity.CredentialPage
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, c := range page.Credentials {
			if c.PrincipalID != "producer" || c.Kind != "service" || seen[c.CredentialID] || c.CredentialID <= after {
				t.Fatal("inventory lost filtering or repeated a credential")
			}
			seen[c.CredentialID] = true
			if c.RevokedAt != nil {
				sawRevoked = c.ExpiresAt != nil && c.ExpiresAt.Before(time.Now()) && c.LastUsedAt != nil
			} else {
				sawCurrent = c.ExpiresAt != nil && c.ExpiresAt.Equal(expires)
			}
		}
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter <= after {
			t.Fatal("nonadvancing inventory cursor")
		}
		after = page.NextAfter
	}
	if !sawRevoked || !sawCurrent {
		t.Fatal("inventory collapsed a principal's historical and current credentials")
	}
	for _, query := range []string{"limit=0", "limit=201", "after=-1", "after=abc", "kind=unknown", "limit=1&limit=2", "typo=device", "kind=device;ignored"} {
		if w := call("/auth/keys?" + query); w.Code != 400 {
			t.Fatalf("invalid inventory query accepted: %s", query)
		}
	}
	if w := call("/auth/keys?principal=absent"); w.Code != 200 || !strings.Contains(w.Body.String(), `"credentials":[]`) {
		t.Fatal("empty inventory response unavailable")
	}
}

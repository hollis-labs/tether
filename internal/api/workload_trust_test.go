package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestWorkloadTrustHTTP(t *testing.T) {
	for _, profile := range []struct {
		audience string
		scopes   []string
	}{
		{authissuer.BasaliskStatusAudience, []string{identity.BasaliskStatusScope}},
		{authissuer.TachyonAssuranceAudience, []string{identity.TachyonCompletionsScope, identity.TachyonItemsScope, identity.TachyonOutcomesScope}},
	} {
		t.Run(profile.audience, func(t *testing.T) {
			ids, db, op, _ := deviceAuthFixture(t)
			ctx := context.Background()
			operator, err := ids.Verify(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			pair := func(scopes ...string) identity.DeviceExchange {
				t.Helper()
				g, err := ids.CreatePairingGrant(ctx, operator, "synthetic workload", scopes, time.Minute, "")
				if err != nil {
					t.Fatal(err)
				}
				d, err := ids.ExchangePairingGrant(ctx, g.Code, nil, "")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ids.ExchangePairingGrant(ctx, g.Code, nil, ""); err == nil {
					t.Fatal("grant replay accepted")
				}
				return d
			}
			workload, reader, broad := pair(profile.scopes...), pair(identity.WorkloadTrustScope), pair(identity.ScopeMaintain)
			dir := t.TempDir()
			keys, err := authissuer.OpenKeyStore(filepath.Join(dir, "keys"), 0)
			if err != nil {
				t.Fatal(err)
			}
			state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			is := authissuer.New("synthetic-issuer", keys, state)
			inner := NewTokenIssuerHandler(is, WithWorkloadTrustStore(ids))
			h := identity.RemoteMiddleware(ids, nil, RemoteScopeMiddleware(inner))
			exchangeJSON, err := json.Marshal(map[string]any{"audience": profile.audience, "scopes": profile.scopes, "ttl_seconds": 300})
			if err != nil {
				t.Fatal(err)
			}
			exchange := string(exchangeJSON)
			code, tok := mint(t, h, workload.Token, exchange)
			if code != 200 {
				t.Fatalf("supported narrow exchange: %d", code)
			}
			parent, err := ids.Verify(ctx, workload.Token)
			if err != nil {
				t.Fatal(err)
			}
			q := WorkloadTrustRequest{QueryID: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Workload: authissuer.WorkloadQuery{Issuer: is.Name, Subject: parent.ID, Kind: parent.Kind, Audience: profile.audience, Scopes: profile.scopes, TokenID: tok.JTI, Parent: parent.ID, ExpiresAt: tok.ExpiresAt.Truncate(time.Second)}}
			query := func(token string, request WorkloadTrustRequest) *httptest.ResponseRecorder {
				t.Helper()
				b, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				return call(h, "POST", WorkloadTrustPath, token, string(b))
			}
			check := func(active bool) {
				t.Helper()
				out := query(reader.Token, q)
				var reply WorkloadTrustResponse
				if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &reply) != nil || reply.Active != active || reply.QueryID != q.QueryID || reply.Workload.TokenID != tok.JTI || reply.CheckedAt.IsZero() {
					t.Fatalf("unexpected trust response: %d %s", out.Code, out.Body)
				}
				if out.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("cacheable trust response")
				}
				for _, secret := range []string{workload.Token, reader.Token, tok.Token, identity.HashToken(reader.Token)} {
					if strings.Contains(out.Body.String(), secret) {
						t.Fatal("secret exposed")
					}
				}
			}
			check(true)
			for range authissuer.MaxWorkloadMintsPerCredential - 1 {
				if status, _ := mint(t, h, workload.Token, exchange); status != 200 {
					t.Fatal("mint capacity reached early")
				}
			}
			limited := call(h, "POST", TokenPath, workload.Token, exchange)
			if limited.Code != 429 || limited.Header().Get("Retry-After") != "60" || limited.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing capacity response")
			}
			for _, token := range []string{reader.Token, broad.Token} {
				deviceHandler := identity.RemoteMiddleware(ids, nil, RemoteScopeMiddleware(NewDeviceAuthHandler(ids)))
				if response := call(deviceHandler, "POST", "/auth/pair", token, `{"scopes":["basalisk-status.assurance_read"]}`); response.Code != 403 {
					t.Fatal("remote device commissioned a workload")
				}
			}
			for _, scopes := range [][]string{{identity.BasaliskStatusScope, "admin"}, {identity.WorkloadTrustScope, "read"}, {identity.WorkloadTrustScope, identity.BasaliskStatusScope}, {"basalisk-status.*"}, {"other.app"}, {identity.TachyonItemsScope, "admin"}, {identity.TachyonItemsScope, identity.BasaliskStatusScope}, {identity.TachyonOutcomesScope, identity.WorkloadTrustScope}, {"tachyon-assurance.*"}} {
				if _, err := ids.CreatePairingGrant(ctx, operator, "invalid", scopes, time.Minute, ""); err == nil {
					t.Fatal("mixed or arbitrary workload scopes commissioned")
				}
			}
			for _, token := range []string{"", op, broad.Token, workload.Token, tok.Token} {
				out := query(token, q)
				if out.Code < 400 {
					t.Fatal("unauthorized reader accepted")
				}
			}
			// Scoped reader has no inventory or workload mint authority (the minimal
			// deny-list is public, so it is not an authority a reader could gain).
			for _, path := range []string{"/auth/keys"} {
				if out := call(h, "GET", path, reader.Token, ""); out.Code < 400 {
					t.Fatal("reader expanded authority")
				}
			}
			if status, _ := mint(t, h, reader.Token, exchange); status != 403 {
				t.Fatal("reader minted status token")
			}
			bad := q
			bad.QueryID = "stale"
			if query(reader.Token, bad).Code != 400 {
				t.Fatal("malformed query id accepted")
			}
			bad = q
			bad.Workload.Parent = "other"
			if query(reader.Token, bad).Code != 400 {
				t.Fatal("delegated parent accepted")
			}
			bad = q
			bad.Workload.TokenID = "unknown"
			out := query(reader.Token, bad)
			var inactive WorkloadTrustResponse
			if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &inactive) != nil || inactive.Active {
				t.Fatal("unknown token active")
			}
			// Every request gets its own echo; concurrent/repeated reads do not consume.
			var wg sync.WaitGroup
			for n := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					qq := q
					nonce := make([]byte, 32)
					nonce[0] = byte(n)
					qq.QueryID = base64.RawURLEncoding.EncodeToString(nonce)
					out := query(reader.Token, qq)
					var reply WorkloadTrustResponse
					if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &reply) != nil || reply.QueryID != qq.QueryID || !reply.Active {
						t.Error("concurrent query confused")
					}
				}()
			}
			wg.Wait()
			check(true)
			if err := ids.RevokeDevice(ctx, parent.ID); err != nil {
				t.Fatal(err)
			}
			check(false)
			if code, _ := mint(t, h, workload.Token, exchange); code < 400 {
				t.Fatal("revoked device exchanged")
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if out := call(inner, "POST", WorkloadTrustPath, reader.Token, `{}`); out.Code != 503 {
				t.Fatal("store outage not unavailable")
			}
		})
	}
}

func TestWorkloadTrustNeverTrustsContextOrBroadScope(t *testing.T) {
	ids, _, op, _ := deviceAuthFixture(t)
	dir := t.TempDir()
	keys, err := authissuer.OpenKeyStore(filepath.Join(dir, "keys"), 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	is := authissuer.New("test", keys, state)
	h := NewTokenIssuerHandler(is, WithWorkloadTrustStore(ids))
	claimed := identity.WithPrincipal(context.Background(), identity.Principal{Verified: true, ID: "claimed", Kind: "device", Scopes: []string{identity.WorkloadTrustScope}})
	r := httptest.NewRequest("POST", WorkloadTrustPath, strings.NewReader(`{}`)).WithContext(claimed)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("context-only reader accepted")
	}
	for _, mode := range []identity.Mode{identity.Off, identity.Observe, identity.Enforce} {
		wrapped := identity.Middleware(mode, ids, nil, h)
		if out := call(wrapped, "POST", WorkloadTrustPath, op, `{}`); out.Code != 403 {
			t.Fatal("operator accepted")
		}
	}
	if out := call(NewTokenIssuerHandler(is), "POST", WorkloadTrustPath, op, `{}`); out.Code != http.StatusServiceUnavailable {
		t.Fatal("missing store accepted")
	}
}

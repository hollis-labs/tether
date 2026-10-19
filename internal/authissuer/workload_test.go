package authissuer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestCurrentWorkloadLifecycle(t *testing.T) {
	for _, profile := range []struct {
		audience string
		scopes   []string
	}{
		{BasaliskStatusAudience, []string{identity.BasaliskStatusScope}},
		{TachyonAssuranceAudience, []string{identity.TachyonCompletionsScope, identity.TachyonItemsScope, identity.TachyonOutcomesScope}},
	} {
		t.Run(profile.audience, func(t *testing.T) {
			for _, scenario := range []string{"active", "credential revoked", "principal revoked", "parent denied", "parent mints denied", "token denied", "scope narrowed", "credential expired", "token expired", "key overlap", "key retired", "restart", "unknown token", "wrong subject", "wrong parent", "wrong kind", "wrong scope", "wrong issuer", "wrong expiry", "missing ledger", "store outage", "same subject replacement"} {
				t.Run(scenario, func(t *testing.T) {
					ctx := context.Background()
					db, err := store.Open(filepath.Join(t.TempDir(), "db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					ids := identity.NewStore(db.DB())
					is, dir := newIssuer(t)
					exp := time.Now().Add(time.Hour)
					raw, err := ids.Mint(ctx, identity.Principal{ID: "msg://service/synthetic", Kind: "service", Scopes: profile.scopes, ExpiresAt: &exp})
					if err != nil {
						t.Fatal(err)
					}
					parent, err := ids.Verify(ctx, raw)
					if err != nil {
						t.Fatal(err)
					}
					tok, err := is.Exchange(parent, Request{Audience: profile.audience, Scopes: profile.scopes})
					if err != nil {
						t.Fatal(err)
					}
					// Exercise the actual shared signature verifier before deriving metadata.
					v, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: is.Name, Audience: profile.audience, JWKSURL: localJWKSURL, Client: &http.Client{Transport: localJWKS{is.Keys}}})
					if err != nil {
						t.Fatal(err)
					}
					p, err := v.Verify(ctx, tok.Token)
					if err != nil {
						t.Fatal(err)
					}
					q := WorkloadQuery{Issuer: is.Name, Subject: p.ID, Kind: p.Kind, Audience: p.Audience, Scopes: p.Scopes, TokenID: p.TokenID, Parent: p.Parent, ExpiresAt: p.ExpiresAt}
					want, wantErr := false, false
					switch scenario {
					case "active":
						want = true
					case "credential revoked":
						err = ids.RevokeToken(ctx, raw)
					case "principal revoked":
						err = ids.Revoke(ctx, parent.ID)
					case "parent denied":
						err = is.State.Deny(DenySub, parent.ID, time.Time{})
					case "parent mints denied":
						_, err = is.State.DenyMintedBy(parent.ID)
					case "token denied":
						err = is.State.Deny(DenyJTI, tok.JTI, tok.ExpiresAt)
					case "scope narrowed":
						_, err = db.DB().Exec(`UPDATE principals SET scopes_json='[]' WHERE id=?`, parent.CredentialID)
					case "credential expired":
						_, err = db.DB().Exec(`UPDATE principals SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).Format(time.RFC3339Nano), parent.CredentialID)
					case "token expired":
						is.Now = func() time.Time { return tok.ExpiresAt }
					case "key overlap":
						err = is.Rotate()
						want = true
					case "key retired":
						err = is.Rotate()
						is.Keys.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
					case "restart":
						is.State, err = OpenState(filepath.Join(dir, "state.json"))
						want = true
					case "unknown token":
						q.TokenID = "unknown"
					case "wrong subject":
						q.Subject = "other"
					case "wrong parent":
						q.Parent = "other"
					case "wrong kind":
						q.Kind = "device"
					case "wrong scope":
						q.Scopes = []string{"*"}
					case "wrong issuer":
						q.Issuer = "other"
					case "wrong expiry":
						q.ExpiresAt = q.ExpiresAt.Add(time.Second)
					case "missing ledger":
						delete(is.State.doc.Workloads, tok.JTI)
					case "store outage":
						err = db.Close()
						wantErr = true
					case "same subject replacement":
						err = ids.RevokeToken(ctx, raw)
						if err == nil {
							_, err = ids.Mint(ctx, identity.Principal{ID: parent.ID, Kind: "service", Scopes: parent.Scopes, ExpiresAt: &exp})
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					active, err := is.CurrentWorkload(ctx, ids, q)
					if active != want || (err != nil) != wantErr {
						t.Fatalf("active=%v err=%v", active, err)
					}
				})
			}
		})
	}
}

func TestCurrentWorkloadMintFailureAndProfile(t *testing.T) {
	is, dir := newIssuer(t)
	p := identity.Principal{Verified: true, CredentialID: 1, ID: "synthetic", Kind: "device", Scopes: []string{identity.BasaliskStatusScope}}
	req := Request{Audience: BasaliskStatusAudience, Scopes: []string{identity.BasaliskStatusScope}}
	for _, change := range []string{"operator", "agent", "delegated", "wildcard", "missing credential", "ttl", "scope", "extra scope"} {
		pp, rr := p, req
		switch change {
		case "extra scope":
			pp.Scopes = []string{identity.BasaliskStatusScope, "admin"}
		case "operator":
			pp.Kind = "operator"
		case "agent":
			pp.Kind = "agent"
		case "delegated":
			rr.SessionID = "session"
		case "wildcard":
			pp.Scopes = []string{"*"}
		case "missing credential":
			pp.CredentialID = 0
		case "ttl":
			rr.TTL = 16 * time.Minute
		case "scope":
			rr.Scopes = []string{"basalisk-status.other"}
		}
		if _, err := is.Exchange(pp, rr); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s accepted: %v", change, err)
		}
	}
	// Persist failure must not return a usable token or leave an eligible record.
	is.State.path = filepath.Join(dir, "absent", "state.json")
	tok, err := is.Exchange(p, req)
	if err == nil || tok.Token != "" || len(is.State.doc.Workloads) != 0 {
		t.Fatal("mint escaped a failed durable record")
	}
	is.State.path = filepath.Join(dir, "state.json")
	tok, err = is.Exchange(p, req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(is.State.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), tok.Token) {
		t.Fatal("persisted bearer")
	}
}

func TestCurrentWorkloadMintCapacity(t *testing.T) {
	is, _ := newIssuer(t)
	p := identity.Principal{Verified: true, CredentialID: 1, ID: "synthetic", Kind: "device", Scopes: []string{identity.BasaliskStatusScope}}
	req := Request{Audience: BasaliskStatusAudience, Scopes: []string{identity.BasaliskStatusScope}}
	var first Token
	for range MaxWorkloadMintsPerCredential {
		tok, err := is.Exchange(p, req)
		if err != nil {
			t.Fatal(err)
		}
		first = tok
	}
	if tok, err := is.Exchange(p, req); !errors.Is(err, ErrWorkloadMintLimit) || tok.Token != "" {
		t.Fatal("per-credential capacity not enforced")
	}
	// Denial still commits at capacity; refusing new mints does not require a write.
	if err := is.State.Deny(DenyJTI, first.JTI, first.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	p.CredentialID = 2
	p.ID = "other"
	if _, err := is.Exchange(p, req); err != nil {
		t.Fatal("one workload exhausted another's capacity")
	}
	// Exercise the global guard without hundreds of disk writes.
	for n := len(is.State.doc.Workloads); n < MaxWorkloadMints; n++ {
		is.State.doc.Workloads[fmt.Sprint(n)] = WorkloadMint{CredentialID: 99, Claims: Claims{Exp: time.Now().Add(time.Hour).Unix()}}
	}
	if tok, err := is.Exchange(p, req); !errors.Is(err, ErrWorkloadMintLimit) || tok.Token != "" {
		t.Fatal("global capacity not enforced")
	}
	is.State.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := is.Exchange(p, req); err != nil {
		t.Fatal("expired records did not release capacity")
	}
}

func TestTachyonWorkloadScopesAndNarrowing(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	is, dir := newIssuer(t)
	exp := time.Now().Add(time.Hour)
	scopes := []string{identity.TachyonCompletionsScope, identity.TachyonItemsScope, identity.TachyonOutcomesScope}
	raw, err := ids.Mint(ctx, identity.Principal{ID: "msg://service/tachyon-caller", Kind: "service", Scopes: scopes, ExpiresAt: &exp})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := ids.Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: is.Name, Audience: TachyonAssuranceAudience, JWKSURL: localJWKSURL, Client: &http.Client{Transport: localJWKS{is.Keys}}})
	if err != nil {
		t.Fatal(err)
	}
	queries := make(map[string]WorkloadQuery)
	for _, scope := range scopes {
		tok, err := is.Exchange(parent, Request{Audience: TachyonAssuranceAudience, Scopes: []string{scope}, TTL: 300 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		p, err := v.Verify(ctx, tok.Token)
		if err != nil {
			t.Fatal(err)
		}
		q := WorkloadQuery{Issuer: is.Name, Subject: p.ID, Kind: p.Kind, Audience: p.Audience, Scopes: p.Scopes, TokenID: p.TokenID, Parent: p.Parent, ExpiresAt: p.ExpiresAt}
		queries[scope] = q
		if active, err := is.CurrentWorkload(ctx, ids, q); err != nil || !active {
			t.Fatalf("route token inactive: %v", err)
		}
		q.Scopes = []string{identity.TachyonOutcomesScope, identity.TachyonItemsScope}
		if active, err := is.CurrentWorkload(ctx, ids, q); err != nil || active {
			t.Fatal("changed claim scopes accepted")
		}
	}
	// A subset grant preserves only still-authorized tokens. Outcome recovery
	// must not retain delivery or item authority after narrowing.
	if _, err := db.DB().Exec(`UPDATE principals SET scopes_json=? WHERE id=?`, `["tachyon-assurance.outcomes_read"]`, parent.CredentialID); err != nil {
		t.Fatal(err)
	}
	for scope, q := range queries {
		active, err := is.CurrentWorkload(ctx, ids, q)
		if err != nil || active != (scope == identity.TachyonOutcomesScope) {
			t.Fatalf("narrowed %s: active=%v err=%v", scope, active, err)
		}
	}
	parent, err = ids.Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := is.Exchange(parent, Request{Audience: TachyonAssuranceAudience, Scopes: []string{identity.TachyonCompletionsScope}}); !errors.Is(err, ErrForbidden) {
		t.Fatal("outcome-only parent delivered")
	}
	if _, err := is.Exchange(parent, Request{Audience: TachyonAssuranceAudience, Scopes: []string{identity.TachyonOutcomesScope}}); err != nil {
		t.Fatal(err)
	}
	// Adding a broad grant makes the credential ineligible for this profile,
	// even when it still contains the requested exact route scope.
	if _, err := db.DB().Exec(`UPDATE principals SET scopes_json=? WHERE id=?`, `["tachyon-assurance.outcomes_read","admin"]`, parent.CredentialID); err != nil {
		t.Fatal(err)
	}
	if active, err := is.CurrentWorkload(ctx, ids, queries[identity.TachyonOutcomesScope]); err != nil || active {
		t.Fatal("mixed broad credential active")
	}
	parent.Scopes = scopes
	for _, change := range []string{"operator", "agent", "issued", "delegated", "wildcard", "mixed", "status", "unknown", "missing credential", "ttl"} {
		pp := parent
		rr := Request{Audience: TachyonAssuranceAudience, Scopes: []string{identity.TachyonItemsScope}}
		switch change {
		case "operator":
			pp.Kind = "operator"
		case "agent":
			pp.Kind = "agent"
		case "issued":
			pp.TokenID = "already-issued"
		case "delegated":
			rr.SessionID = "session"
		case "wildcard":
			pp.Scopes = []string{"tachyon-assurance.*"}
		case "mixed":
			pp.Scopes = append([]string{"admin"}, scopes...)
		case "status":
			pp.Scopes = []string{identity.BasaliskStatusScope}
		case "unknown":
			rr.Scopes = []string{"tachyon-assurance.other"}
		case "missing credential":
			pp.CredentialID = 0
		case "ttl":
			rr.TTL = 16 * time.Minute
		}
		if _, err := is.Exchange(pp, rr); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s accepted: %v", change, err)
		}
	}
	// The new audience shares the durable release boundary with status tokens.
	is.State.path = filepath.Join(dir, "absent", "state.json")
	if tok, err := is.Exchange(parent, Request{Audience: TachyonAssuranceAudience, Scopes: scopes}); err == nil || tok.Token != "" {
		t.Fatal("Tachyon token escaped persistence failure")
	}
}

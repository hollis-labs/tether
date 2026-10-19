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
			raw, err := ids.Mint(ctx, identity.Principal{ID: "msg://service/synthetic", Kind: "service", Scopes: []string{identity.BasaliskStatusScope}, ExpiresAt: &exp})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := ids.Verify(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			tok, err := is.Exchange(parent, Request{Audience: BasaliskStatusAudience, Scopes: []string{identity.BasaliskStatusScope}})
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the actual shared signature verifier before deriving metadata.
			v, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: is.Name, Audience: BasaliskStatusAudience, JWKSURL: localJWKSURL, Client: &http.Client{Transport: localJWKS{is.Keys}}})
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

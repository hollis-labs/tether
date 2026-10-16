package api

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// localCtx returns a context carrying proof of an accepted unix connection.
func localCtx(t *testing.T) context.Context {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	done := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); done <- c }()
	cl, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	srv := <-done
	t.Cleanup(func() { _ = srv.Close() })
	return identity.ConnectionContext(context.Background(), srv)
}

func reqAs(ctx context.Context, p identity.Principal) *http.Request {
	return httptest.NewRequest("GET", "/x", nil).WithContext(identity.WithPrincipal(ctx, p))
}

// An operator-minted narrow token carries the operator's id and kind but is
// not the operator: every operator-only gate must refuse it.
func TestOperatorMintedNarrowTokenIsNotTheOperator(t *testing.T) {
	ctx := localCtx(t)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	narrow := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"tether.read"}, TokenID: "jti-1", Audience: "tether", Parent: identity.OperatorID}

	if _, ok := localOperator(reqAs(ctx, op)); !ok {
		t.Fatal("the real operator must pass localOperator (/auth/pair)")
	}
	if _, ok := localOperator(reqAs(ctx, narrow)); ok {
		t.Fatal("narrow token passed localOperator: /auth/pair would mint a durable admin device")
	}
	for _, write := range []bool{false, true} {
		if !denyListReader(reqAs(ctx, op), write) {
			t.Fatalf("operator denied deny-list write=%v", write)
		}
		if denyListReader(reqAs(ctx, narrow), write) {
			t.Fatalf("narrow token passed deny-list write=%v", write)
		}
	}
	if !catalogApprover(reqAs(ctx, op)) || catalogApprover(reqAs(ctx, narrow)) {
		t.Fatal("catalogApprover must accept only the real operator")
	}
	if !identity.IsOperatorCredential(op) || identity.IsOperatorCredential(narrow) {
		t.Fatal("IsOperatorCredential")
	}
}

func TestIssuedTokensNeedCatalogWriteScope(t *testing.T) {
	ctx := localCtx(t)
	for name, p := range map[string]identity.Principal{
		"agent":           {Verified: true, Kind: "agent", ID: "msg://session/local/s1", TokenID: "j", Scopes: []string{"tether.messages"}},
		"session":         {Verified: true, Kind: "session", ID: "msg://session/local/s1", Scopes: []string{"read"}},
		"operator-minted": {Verified: true, Kind: "operator", ID: identity.OperatorID, TokenID: "j", Scopes: []string{"tether.read"}},
	} {
		if catalogWriter(reqAs(ctx, p)) {
			t.Errorf("%s: catalogWriter allowed without catalog.write", name)
		}
		if reloadCaller(reqAs(ctx, p)) {
			t.Errorf("%s: reloadCaller allowed without catalog.write", name)
		}
		p.Scopes = append(p.Scopes, catalogWriteScope)
		if !catalogWriter(reqAs(ctx, p)) || !reloadCaller(reqAs(ctx, p)) {
			t.Errorf("%s: catalog.write holder refused", name)
		}
	}
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	if !catalogWriter(reqAs(ctx, op)) || !reloadCaller(reqAs(ctx, op)) {
		t.Fatal("real operator must keep catalog access")
	}
}

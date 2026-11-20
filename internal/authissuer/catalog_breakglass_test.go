package authissuer

import (
	"github.com/hollis-labs/tether/internal/identity"
	"testing"
	"time"
)

func TestCatalogBreakglassExistingKeyRestartRotation(t *testing.T) {
	issuer, dir := newIssuer(t)
	count := len(issuer.Keys.KIDs())
	body := []byte(`{"principal":"msg://agent/test/helper"}`)
	signature, err := issuer.Keys.SealCatalogBreakglass(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(issuer.Keys.KIDs()) != count {
		t.Fatal("sealing generated another key")
	}
	reopened, err := OpenKeyStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.VerifyCatalogBreakglass(body, signature); err != nil {
		t.Fatal(err)
	}
	if err := reopened.VerifyCatalogBreakglass([]byte("changed"), signature); err == nil {
		t.Fatal("tampered body verified")
	}
	if err := issuer.Rotate(); err != nil {
		t.Fatal(err)
	}
	if err := issuer.Keys.VerifyCatalogBreakglass(body, signature); err == nil {
		t.Fatal("old active key kept breakglass alive after rotation")
	}
}

func TestCatalogApproveScopeMintingIsSeparate(t *testing.T) {
	issuer, _ := newIssuer(t)
	req := ServiceRequest{App: "custom", Audience: "tether", Scopes: []string{"catalog.approve"}}
	token, _, err := issuer.IssueService(operatorP, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(token.Scopes) != 1 || token.Scopes[0] != "catalog.approve" {
		t.Fatalf("scope changed: %v", token.Scopes)
	}
	for _, scope := range []string{"catalog.write", "maintain", "tether.catalog"} {
		if identity.ScopeGranted([]string{scope}, "catalog.approve") {
			t.Fatalf("%s gained approval authority", scope)
		}
	}
	if _, _, err := issuer.IssueService(identity.Principal{Verified: true, Kind: "agent", ID: "msg://agent/test/helper", Scopes: []string{"*"}}, req); err == nil {
		t.Fatal("agent minted approval capability")
	}
}

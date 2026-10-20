package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/authissuer"
)

func serviceTokenFake(t *testing.T) (deviceAdminFactory, *api.ServiceTokenRequest) {
	t.Helper()
	var got api.ServiceTokenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(api.ServiceTokenResponse{
				Token:  authissuer.Token{Token: "header.payload.signature-secret", Kind: "service", JTI: "j1"},
				Record: authissuer.ServiceRecord{JTI: "j1", Sub: "msg://service/cerberus/dev", Audience: "tether", Scopes: []string{"tether.events"}, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)},
			})
		default:
			_ = json.NewEncoder(w).Encode(api.ServiceTokenListResponse{Tokens: []authissuer.ServiceRecord{{JTI: "j1", Sub: "msg://service/cerberus/dev", Audience: "tether", Scopes: []string{"tether.events"}, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Label: "agent-os"}}})
		}
	}))
	t.Cleanup(srv.Close)
	return func() (*deviceAdminClient, error) {
		return &deviceAdminClient{client: srv.Client(), baseURL: srv.URL, token: "op"}, nil
	}, &got
}

func TestServiceTokenIssueWritesA0600FileAndNeverPrintsIt(t *testing.T) {
	factory, got := serviceTokenFake(t)
	cmd := newServiceTokenCommand(factory)
	file := filepath.Join(t.TempDir(), "cerberus.token")
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"issue", "--app", "cerberus", "--scope", "events", "--label", "agent-os", "--out-file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.App != "cerberus" || got.Audience != "tether" || len(got.Scopes) != 1 || got.Scopes[0] != "events" || got.Label != "agent-os" {
		t.Fatalf("request %+v", got)
	}
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", err, fi)
	}
	data, _ := os.ReadFile(file)
	if strings.TrimSpace(string(data)) != "header.payload.signature-secret" {
		t.Fatalf("file %q", data)
	}
	if strings.Contains(out.String()+errb.String(), "signature-secret") {
		t.Fatal("the token was printed")
	}
	// A second issue to the same file is refused rather than overwriting a credential.
	cmd2 := newServiceTokenCommand(factory)
	cmd2.SetArgs([]string{"issue", "--app", "cerberus", "--scope", "events", "--out-file", file})
	cmd2.SetOut(&out)
	cmd2.SetErr(&errb)
	if err := cmd2.Execute(); err == nil {
		t.Fatal("overwrote an existing token file")
	}
}

func TestServiceTokenListShowsMetadataOnly(t *testing.T) {
	factory, _ := serviceTokenFake(t)
	cmd := newServiceTokenCommand(factory)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "msg://service/cerberus/dev") || !strings.Contains(out.String(), "agent-os") || strings.Contains(out.String(), "signature") {
		t.Fatalf("%s", out.String())
	}
}

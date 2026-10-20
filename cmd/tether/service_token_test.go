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
		if strings.HasSuffix(r.URL.Path, "/rotate-key") {
			var rq api.ServiceKeyRotateRequest
			_ = json.NewDecoder(r.Body).Decode(&rq)
			rec := authissuer.ServiceRecord{JTI: "old1", Sub: "msg://service/cerberus/dev", App: "cerberus", Env: "dev", Label: "agent-os"}
			out := api.ServiceKeyRotateResponse{NewKID: "svc-new", Pending: []authissuer.ServiceRecord{rec, {JTI: "old2", Sub: "msg://service/nanite/dev", App: "nanite", Env: "dev"}}}
			if rq.Reissue {
				out.Reissued = []api.ServiceTokenResponse{{Token: authissuer.Token{Token: "new.token.value-secret"}, Record: authissuer.ServiceRecord{JTI: "new1aaaa-bbbb", Sub: rec.Sub, App: "cerberus", Env: "dev", Replaces: "old1"}}}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
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

func TestServiceTokenRevokeArgumentRules(t *testing.T) {
	factory, _ := serviceTokenFake(t)
	for _, args := range [][]string{{"revoke"}, {"revoke", "j1", "--app", "cerberus"}} {
		cmd := newServiceTokenCommand(factory)
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestServiceTokenRotateKeyWritesReplacementsToFilesAndReportsTheRest(t *testing.T) {
	factory, _ := serviceTokenFake(t)
	// Re-issue needs somewhere private to put the tokens: they are never printed.
	cmd := newServiceTokenCommand(factory)
	cmd.SetArgs([]string{"rotate-key", "--reissue"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("--reissue without --out-dir accepted")
	}
	cmd = newServiceTokenCommand(factory)
	cmd.SetArgs([]string{"rotate-key", "--out-dir", t.TempDir()})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("--out-dir without --reissue accepted")
	}

	dir := filepath.Join(t.TempDir(), "out")
	var out, errb bytes.Buffer
	cmd = newServiceTokenCommand(factory)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"rotate-key", "--reissue", "--out-dir", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("%d files", len(files))
	}
	fi, _ := files[0].Info()
	data, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if fi.Mode().Perm() != 0o600 || strings.TrimSpace(string(data)) != "new.token.value-secret" || !strings.HasPrefix(files[0].Name(), "cerberus-dev-new1aaaa") {
		t.Fatalf("%s %v %q", files[0].Name(), fi.Mode().Perm(), data)
	}
	if strings.Contains(out.String()+errb.String(), "value-secret") {
		t.Fatal("a replacement token was printed")
	}
	// The record that was NOT re-issued is reported as needing re-delivery.
	if !strings.Contains(out.String(), "needs re-delivery: msg://service/nanite/dev jti old2") || strings.Contains(out.String(), "needs re-delivery: msg://service/cerberus") ||
		!strings.Contains(out.String(), "new kid svc-new") {
		t.Fatalf("%s", out.String())
	}
	// A second run into the same directory never overwrites a credential file.
	cmd = newServiceTokenCommand(factory)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"rotate-key", "--reissue", "--out-dir", dir})
	if err := cmd.Execute(); err == nil {
		t.Fatal("overwrote an existing token file")
	}
}

func TestServiceTokenRevokeAllArgumentRules(t *testing.T) {
	factory, _ := serviceTokenFake(t)
	for _, args := range [][]string{{"revoke", "--all", "j1"}, {"revoke", "--all", "--env", "dev"}, {"revoke", "--all", "--app", "x", "--env", "dev"}} {
		cmd := newServiceTokenCommand(factory)
		cmd.SetArgs(args)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

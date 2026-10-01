package main

import (
	"context"
	"github.com/hollis-labs/tether/internal/identity"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCLICallerCredentialPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_TOKEN", "")
	oldCatalog, oldFile := catalogPath, tokenFilePath
	t.Cleanup(func() { catalogPath = oldCatalog; tokenFilePath = oldFile })
	catalogPath = filepath.Join(home, "custom", "catalog")
	tokenFilePath = ""
	defaultToken, _ := identity.NewToken()
	envToken, _ := identity.NewToken()
	fileToken, _ := identity.NewToken()
	defaultFile := filepath.Join(home, "custom", "run", "operator.token")
	explicit := filepath.Join(home, "explicit.token")
	if err := identity.WriteTokenFile(defaultFile, defaultToken); err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(explicit, fileToken); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, env, file, want string }{
		{"default", "", "", defaultToken}, {"environment", envToken, "", envToken}, {"explicit", envToken, explicit, fileToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TETHER_TOKEN", tc.env)
			tokenFilePath = tc.file
			token, err := callerToken()
			if err != nil || token != tc.want {
				t.Fatal("wrong credential source", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+tc.want {
					t.Error("CLI client selected wrong bearer")
				}
				_, _ = io.WriteString(w, `{"status":"ok"}`)
			}))
			defer server.Close()
			if err := daemonClient("tcp:" + strings.TrimPrefix(server.URL, "http://")).Ping(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
	tokenFilePath = explicit
	t.Setenv("TETHER_TOKEN", envToken)
	if err := os.Chmod(explicit, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callerToken(); err == nil {
		t.Fatal("bad explicit file fell back to environment")
	}
	tokenFilePath = filepath.Join(home, "missing")
	if _, err := callerToken(); err == nil {
		t.Fatal("missing explicit file fell back")
	}
}

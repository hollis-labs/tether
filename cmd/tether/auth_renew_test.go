package main

import (
	"bytes"
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/secretsetup"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestAuthRenewCommandDoesNotPrintCredential(t *testing.T) {
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "device.token")
	if err := os.WriteFile(file, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{200, 401} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != "tether.example.invalid" {
				t.Error("allowed Host override missing")
			}
			w.WriteHeader(status)
			if status == 200 {
				fmt.Fprintf(w, `{"expires_at":%q,"token":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339), token)
			} else {
				fmt.Fprint(w, token)
			}
		}))
		cmd := newDeviceRenewCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--hub", server.URL, "--host", "tether.example.invalid", "--credential-file", file})
		err := cmd.Execute()
		server.Close()
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d error %v", status, err)
		}
		if strings.Contains(out.String(), token) || (err != nil && strings.Contains(err.Error(), token)) {
			t.Fatal("credential exposed")
		}
	}
}
func TestCredentialDoctorWarnsForExpiryAndDenyListFailure(t *testing.T) {
	h := daemon.Health{Identity: &daemon.IdentityHealth{DeviceExpiry: &daemon.DeviceExpiryHealth{DeviceExpiryCounts: identity.DeviceExpiryCounts{Expiring: 1}}}, DenyListWatch: &daemon.DenyListWatchHealth{WarningCount: 2, AcknowledgedCount: 1}}
	results := credentialHealthChecks(h)
	if len(results) != 2 {
		t.Fatal(results)
	}
	// The rendered results must carry both risks without requiring identity labels.
	raw := fmt.Sprint(results)
	if !strings.Contains(raw, "expiring within 7d=1") || !strings.Contains(raw, "unacknowledged poll failures=1") {
		t.Fatal(raw)
	}
}

func TestAuthRenewManagedReferenceUsesXDGStateAndKeepsBearer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	ref := "file://tesseract/denylist-bearer"
	resolver := secretsetup.New()
	if err := resolver.Set(context.Background(), ref, strings.NewReader(token)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("managed credential mismatch")
		}
		fmt.Fprintf(w, `{"expires_at":%q}`, time.Now().Add(24*time.Hour).Format(time.RFC3339))
	}))
	defer server.Close()
	cmd := newDeviceRenewCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--hub", server.URL, "--credential-ref", ref})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	stored, err := resolver.Resolve(context.Background(), ref)
	if err != nil || stored.Secret.Reveal() != token || strings.Contains(out.String(), token) {
		t.Fatal("renewal changed or exposed bearer")
	}
}

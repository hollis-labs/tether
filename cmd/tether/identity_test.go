package main

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIdentityResolutionDoesNotValidateStartupPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"Observe", "typo", "enforce"} {
		cat := &config.Catalog{Global: config.Global{Identity: config.IdentityConfig{Mode: mode}, Daemon: config.DaemonConfig{ListenAddr: "tcp:0.0.0.0:8787", ShutdownTimeout: "5s"}}}
		if err := cat.Validate(); err != nil {
			t.Fatal("shared catalog validation rejected mode", err)
		}
		cfg, err := daemonConfigFromCatalog(cat)
		if err != nil {
			t.Fatal("shared resolver rejected mode/listener", err)
		}
		if mode == "Observe" && cfg.IdentityMode != identity.Observe {
			t.Fatal("mode not normalized")
		}
		if mode == "typo" && cfg.IdentityMode.Validate() == nil {
			t.Fatal("startup accepts invalid mode")
		}
		if mode == "typo" && checkIdentity(cat).Status != "fail" {
			t.Fatal("doctor did not flag bad mode")
		}
	}
}
func TestIdentityOperatorMismatchDegradesObserveOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "run", "operator.token")
	if err := ids.EnsureOperator(ctx, file); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	degraded, err := bootstrapOperator(ctx, ids, file, identity.Observe)
	if err != nil || !degraded {
		t.Fatal("observe startup unavailable after lost token", err)
	}
	if _, err := bootstrapOperator(ctx, ids, file, identity.Enforce); err == nil {
		t.Fatal("enforce accepted missing token")
	}
	wrong, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := identity.WriteTokenFile(file, wrong); err != nil {
		t.Fatal(err)
	}
	degraded, err = bootstrapOperator(ctx, ids, file, identity.Observe)
	if err != nil || !degraded {
		t.Fatal("observe startup unavailable after DB/file mismatch", err)
	}
	if _, err := bootstrapOperator(ctx, ids, file, identity.Enforce); err == nil {
		t.Fatal("enforce accepted mismatch")
	}
}
func TestIdentityDoctorReportsDegradedAndAuditDrops(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	health := daemon.Health{Status: "ok", Identity: &daemon.IdentityHealth{Mode: identity.Observe, OperatorDegraded: true}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(health) }))
	defer server.Close()
	cat := &config.Catalog{Global: config.Global{Identity: config.IdentityConfig{Mode: "observe"}, Daemon: config.DaemonConfig{ListenAddr: "tcp:" + strings.TrimPrefix(server.URL, "http://"), ShutdownTimeout: "5s"}}}
	if result := checkIdentity(cat); result.Status != "warn" || !strings.Contains(result.Message, "degraded") {
		t.Fatal("doctor missed degraded credentials", result)
	}
	health.Identity.OperatorDegraded = false
	health.Identity.Audit.Dropped = 4
	if result := checkIdentity(cat); result.Status != "warn" || !strings.Contains(result.Message, "dropped=4") {
		t.Fatal("doctor missed audit overflow", result)
	}
}

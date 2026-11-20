package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
)

func TestCatalogApprovalEnvironmentAndStartupLog(t *testing.T) {
	for _, raw := range []string{"", "on", "off", " OFF ", "false", "invalid"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("TETHER_CATALOG_APPROVAL", raw)
			cat, err := config.LoadLayered("../../examples/catalog")
			if err != nil {
				t.Fatal(err)
			}
			cat.Global.Catalog.Defaults.StateDB = filepath.Join(t.TempDir(), "state.db")
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			engine := newCatalogApplyEngine(&app.Service{Catalog: cat, CatalogRoot: t.TempDir()})
			off := strings.EqualFold(strings.TrimSpace(raw), "off")
			if engine.ApprovalDisabled() != off {
				t.Fatalf("raw %q mode=%v", raw, engine.ApprovalDisabled())
			}
			want := "catalog approval: on"
			if off {
				want = "catalog approval: off"
			}
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("startup: %s", logs.String())
			}
		})
	}
}

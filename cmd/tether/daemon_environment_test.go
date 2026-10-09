package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/store"
)

func TestEnvironmentUsesOpenedSelectedStateDatabase(t *testing.T) {
	stateDir := t.TempDir()
	db, err := store.Open(filepath.Join(stateDir, "selected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	otherDir := t.TempDir()
	cat := &config.Catalog{Global: config.Global{Catalog: config.CatalogRoots{Defaults: config.Defaults{StateDB: filepath.Join(otherDir, "unselected.db")}}, Environment: config.EnvironmentConfig{Label: "worker", Authority: "worker-1"}}}
	d, err := buildEnvironmentDescriptor(cat, db)
	if err != nil {
		t.Fatal(err)
	}
	id, _, protocol := d.Identity()
	if id == "" || protocol != environment.Protocol {
		t.Fatal("missing environment identity")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "environment-id")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(otherDir, "environment-id")); !os.IsNotExist(err) {
		t.Fatal("identity created next to a database that was not opened")
	}
	again, err := buildEnvironmentDescriptor(cat, db)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _, _ := again.Identity()
	if otherID != id {
		t.Fatal("daemon recomposition changed identity")
	}
}

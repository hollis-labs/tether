package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/events"
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
	d, err := buildEnvironmentDescriptor(cat, db, nil)
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
	again, err := buildEnvironmentDescriptor(cat, db, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherID, _, _ := again.Identity()
	if otherID != id {
		t.Fatal("daemon recomposition changed identity")
	}
}

func TestEnvironmentCapabilitiesFollowInstalledComposition(t *testing.T) {
	profile, err := environment.ResolveProfile("worker", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	withoutLive := composedEnvironmentCapabilities(profile, nil, false)
	if withoutLive["streams"]["environment_events"] != nil || withoutLive["streams"]["environment_snapshot"] != true || withoutLive["raw_attach"] != nil {
		t.Fatal("unavailable capability advertised", withoutLive)
	}
	bus := events.NewBus(events.BusOptions{})
	live := composedEnvironmentCapabilities(profile, bus, true)
	if live["streams"]["environment_events"] != true || live["raw_attach"]["resume"] != true {
		t.Fatal("installed capability missing", live)
	}
	disabled, err := environment.ResolveProfile("worker", map[string]bool{environment.StreamAPI: false, environment.SessionCore: false}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	caps := composedEnvironmentCapabilities(disabled, bus, true)
	if caps["streams"] != nil || caps["raw_attach"] != nil {
		t.Fatal("disabled capability advertised", caps)
	}
}

func TestEnvironmentDescriptorAdvertisesCachedCoarseReport(t *testing.T) {
	profile, err := environment.ResolveProfile("worker", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	caps := composedEnvironmentCapabilities(profile, nil, false)
	d, err := environment.NewDescriptor(environment.Descriptor{EnvironmentID: "synthetic-id", Label: "worker", Capabilities: caps})
	if err != nil {
		t.Fatal(err)
	}
	// Later changes cannot reveal details or change the public cached body.
	caps["capability_report"]["providers"] = "secret provider login"
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest(http.MethodGet, environment.DescriptorPath, nil))
	var got environment.Descriptor
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	group := got.Capabilities["capability_report"]
	if group["providers"] != true || group["resources"] != true || group["sandbox"] != true || group["role_profile"] != true {
		t.Fatal("missing coarse report contract", group)
	}
	for _, detail := range []string{"claude", "codex", "logged_in", "launch_host_shim", "reflink_supported", "state_root", "cpu_load", "enabled_modules"} {
		if group[detail] != nil {
			t.Fatal("public report exposed host detail", detail)
		}
	}
	r := httptest.NewRequest(http.MethodGet, environment.DescriptorPath, nil)
	r.Header.Set("If-None-Match", w.Header().Get("ETag"))
	cached := httptest.NewRecorder()
	d.ServeHTTP(cached, r)
	if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatal("descriptor not cached")
	}
}

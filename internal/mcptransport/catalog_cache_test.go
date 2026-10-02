package mcptransport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestTransportCatalogCacheTracksLayersAndRefusesBrokenGeneration(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	global := filepath.Join(root, "global.yaml")
	if err := os.WriteFile(global, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := &CatalogCache{Root: root}
	first, err := cache.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Load(context.Background())
	if err != nil || first != second {
		t.Fatal("unchanged catalog reparsed", err)
	}
	dir := filepath.Join(home, ".tether", "agents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.yaml"), []byte("id: added\n"), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := cache.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := third.Agents["added"]; !ok {
		t.Fatal("new layer ignored")
	}
	if err := os.WriteFile(global, []byte("identity: [broken\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(context.Background()); err == nil {
		t.Fatal("broken generation served stale grants")
	}
	if err := os.WriteFile(global, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(context.Background()); err != nil {
		t.Fatal("restored catalog failed", err)
	}
}

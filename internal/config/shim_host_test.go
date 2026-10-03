package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShimHostFoundationConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "global.yaml"), []byte("catalog:\n  defaults:\n    shim_host:\n      journal_bytes: 536870912\n      systemd_user: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cat, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cat.Global.Catalog.Defaults.ShimHost.EffectiveJournalBytes() != 512<<20 || !cat.Global.Catalog.Defaults.ShimHost.SystemdUser {
		t.Fatal("shim host config not parsed")
	}
	empty := ShimHostConfig{}
	if empty.EffectiveJournalBytes() != 256<<20 || empty.SystemdUser {
		t.Fatal("unsafe shim host defaults")
	}
}

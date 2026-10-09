package userprofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAbsentRecordAndPersistedLocalProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "user-profile.json")
	p, err := Load(path)
	if err != nil || p.DefaultSender() != OperatorURN || p.Messaging.FromDefault != "" {
		t.Fatalf("absent profile: %+v %v", p, err)
	}
	p.URN = "msg://user/local/chris"
	p.Aliases = []Alias{{URN: p.URN, Alias: "chris"}, {URN: "msg://user/agent-mux/chris", Alias: "chris-mux"}}
	a, err := p.Resolve("@CHRIS-MUX")
	if err != nil {
		t.Fatal(err)
	}
	p.Messaging.FromDefault = a.URN()
	if err := Save(path, p); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil || reloaded.URN != p.URN || reloaded.DefaultSender() != "msg://user/agent-mux/chris" {
		t.Fatalf("reload: %+v %v", reloaded, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("profile mode: %v %v", info, err)
	}
	// Invalid updates cannot replace the last usable preference.
	p.Messaging.FromDefault = "@chris"
	if err := Save(path, p); err == nil {
		t.Fatal("noncanonical saved preference accepted")
	}
	still, err := Load(path)
	if err != nil || still.DefaultSender() != reloaded.DefaultSender() {
		t.Fatal("invalid update replaced preference")
	}
}

func TestInvalidSavedValueIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user-profile.json")
	if err := os.WriteFile(path, []byte(`{"urn":"msg://user/local/chris","aliases":[],"messaging":{"from_default":"unknown"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "messaging.from_default") {
		t.Fatalf("invalid preference silently remapped: %v", err)
	}
}

func TestPathUsesXDGDataDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	path, err := Path()
	if err != nil || path != filepath.Join(root, "XDG_DATA_HOME", "tether", "user-profile.json") {
		t.Fatalf("profile path: %s %v", path, err)
	}
}

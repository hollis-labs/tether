package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type recordingResolver struct {
	resolved []string
	failRef  string
}

func (r *recordingResolver) Resolve(_ context.Context, ref string) (string, error) {
	r.resolved = append(r.resolved, ref)
	if ref == r.failRef {
		return "", errors.New("keychain unavailable")
	}
	return "secret-for-" + ref, nil
}

func writeMCPServers(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mcp-servers"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, "mcp-servers", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ids(entries []MCPServerEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.ID)
	}
	slices.Sort(out)
	return out
}

// CW-20261001-0227: a confined proxy loads, and resolves secrets for, only
// the upstreams it was granted.
func TestLoadMCPServersConfined(t *testing.T) {
	dir := writeMCPServers(t, map[string]string{
		"torque.yaml":    "id: torque\ntransport: stdio\ncommand: torque-mcp\n",
		"tesseract.yaml": "id: tesseract\ntransport: stdio\ncommand: tess\nenv:\n  TOKEN: keychain://tesseract/token\n",
		"cerberus.yaml":  "id: cerberus\ntransport: stdio\ncommand: cerberus\nargs: [--key, keychain://cerberus/key]\n",
		"nanite.yaml":    "id: nanite\ntransport: stdio\ncommand: nanite\nenabled: false\n",
	})
	rec := &recordingResolver{}
	withResolver(t, rec)

	got, unknown, err := LoadMCPServersConfined(dir, []string{"torque", "tesseract"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{"tesseract", "torque"}) {
		t.Fatalf("loaded %q; want only the granted upstreams", ids(got))
	}
	if len(unknown) != 0 {
		t.Fatalf("unknown = %q", unknown)
	}
	// Only the granted upstream's secret was resolved. cerberus's credential
	// never reached this process.
	if !slices.Equal(rec.resolved, []string{"keychain://tesseract/token"}) {
		t.Fatalf("resolved secrets %q; want only tesseract's", rec.resolved)
	}
	for _, e := range got {
		if e.ID == "tesseract" && e.Env["TOKEN"] != "secret-for-keychain://tesseract/token" {
			t.Fatalf("tesseract's token not resolved: %q", e.Env["TOKEN"])
		}
	}
}

// A secret reference of an upstream outside the grant that cannot be resolved
// must not stop the proxy: LoadMCPServers would fail on it.
func TestLoadMCPServersConfined_ExcludedBrokenSecretIsIgnored(t *testing.T) {
	dir := writeMCPServers(t, map[string]string{
		"torque.yaml":   "id: torque\ntransport: stdio\ncommand: torque-mcp\n",
		"cerberus.yaml": "id: cerberus\ntransport: stdio\ncommand: cerberus\nargs: [keychain://cerberus/key]\n",
	})
	withResolver(t, &recordingResolver{failRef: "keychain://cerberus/key"})

	if _, err := LoadMCPServers(dir); err == nil {
		t.Fatal("LoadMCPServers succeeded with an unresolvable secret; the comparison below proves nothing")
	}
	got, _, err := LoadMCPServersConfined(dir, []string{"torque"})
	if err != nil {
		t.Fatalf("confined load failed on an excluded upstream's secret: %v", err)
	}
	if !slices.Equal(ids(got), []string{"torque"}) {
		t.Fatalf("loaded %q", ids(got))
	}
	// A granted upstream's own broken secret is still a hard error.
	if _, _, err := LoadMCPServersConfined(dir, []string{"cerberus"}); err == nil {
		t.Fatal("a granted upstream with an unresolvable secret loaded")
	}
}

func TestLoadMCPServersConfined_UnknownAndDisabledAreReportedAndSkipped(t *testing.T) {
	dir := writeMCPServers(t, map[string]string{
		"torque.yaml": "id: torque\ntransport: stdio\ncommand: torque-mcp\n",
		"nanite.yaml": "id: nanite\ntransport: stdio\ncommand: nanite\nenabled: false\n",
	})
	withResolver(t, &recordingResolver{})

	got, unknown, err := LoadMCPServersConfined(dir, []string{"torque", "nanite", "typo", "typo"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids(got), []string{"torque"}) {
		t.Fatalf("loaded %q", ids(got))
	}
	// A disabled upstream and a name that is not in the catalog are both
	// reported, once each, in the order given.
	if !slices.Equal(unknown, []string{"nanite", "typo"}) {
		t.Fatalf("unknown = %q; want [nanite typo]", unknown)
	}
}

// An empty grant loads nothing: confining to no upstreams is not "all".
func TestLoadMCPServersConfined_EmptyGrantLoadsNothing(t *testing.T) {
	dir := writeMCPServers(t, map[string]string{"torque.yaml": "id: torque\ntransport: stdio\ncommand: torque-mcp\n"})
	withResolver(t, &recordingResolver{})
	got, unknown, err := LoadMCPServersConfined(dir, nil)
	if err != nil || len(got) != 0 || len(unknown) != 0 {
		t.Fatalf("loaded %q, unknown %q, err %v; want nothing", ids(got), unknown, err)
	}
}

func TestLoadMCPServersConfined_MissingCatalogDirIsEmpty(t *testing.T) {
	withResolver(t, &recordingResolver{})
	got, unknown, err := LoadMCPServersConfined(t.TempDir(), []string{"torque"})
	if err != nil || len(got) != 0 || !slices.Equal(unknown, []string{"torque"}) {
		t.Fatalf("loaded %q, unknown %q, err %v", ids(got), unknown, err)
	}
}

func TestMCPDeclaredToolPrefixIsVerbatimCatalogMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "alpha.yaml"), []byte("id: alpha\ntransport: stdio\ncommand: ignored\ntool_prefix: alpha_\n"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := LoadMCPServerCatalog(root)
	if err != nil || len(entries) != 1 || entries[0].ToolPrefix != "alpha_" {
		t.Fatalf("prefix=%+v %v", entries, err)
	}
}

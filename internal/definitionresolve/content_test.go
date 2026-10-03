//go:build unix

package definitionresolve

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/agentdef"
)

func writeContent(t *testing.T, root, name, data string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
func localProvider(t *testing.T, root string) *LocalContent {
	t.Helper()
	p, err := NewLocalContent(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}
func TestContentProtocolVectors(t *testing.T) {
	vectors := []struct {
		name, kind, want string
		files            map[string]string
		mode             os.FileMode
	}{
		{"single", FileContent, "281759411ab44ce3c95b286dde1e290ae5d8038a71f8f1d36de7a16c365f7034", map[string]string{"single": "hello\n"}, 0o644},
		{"tree", TreeContent, "d70b7d116246604337994c68c2ae0dd816d577f580a053787b99375cf6387637", map[string]string{"tree/.note": "dot\n", "tree/bin/run": "#!/bin/sh\nexit 0\n"}, 0o755},
		{"renamed", TreeContent, "bf607365e4c81839baaf0945490ada07a7ed7f8b132257c121afb0f28a1afd85", map[string]string{"renamed/.renamed": "dot\n", "renamed/bin/run": "#!/bin/sh\nexit 0\n"}, 0o755},
		{"no-exec", TreeContent, "66160fd8a572a3b83150af70e45f438fb02c60f7b0d5133e3b78de40755d88e9", map[string]string{"no-exec/.note": "dot\n", "no-exec/bin/run": "#!/bin/sh\nexit 0\n"}, 0o644},
		{"sort", TreeContent, "ab1d89f0c17fd11ffaec326df4dafcdd8079ef55d55ca75832e79a90cdec2f3b", map[string]string{"sort/B": "upper\n", "sort/a.b": "dot-path\n", "sort/a/b": "slash-path\n"}, 0o644},
	}
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			root := t.TempDir()
			for path, data := range v.files {
				mode := os.FileMode(0o644)
				if filepath.Base(path) == "run" {
					mode = v.mode
				}
				writeContent(t, root, path, data, mode)
			}
			p := localProvider(t, root)
			pin, err := p.Pin(t.Context(), "catalog:"+v.name)
			if err != nil || pin.Kind != v.kind || pin.Digest != "sha256:"+v.want {
				t.Fatal(pin, err)
			}
		})
	}
}
func TestContentIgnoredMetadataAndSensitiveContent(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "tree/file", "bytes", 0o644)
	p := localProvider(t, root)
	before, err := p.Pin(t.Context(), "catalog:tree")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "tree", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(root, "tree", "file"), timestamp, timestamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "tree", "file"), 0o655); err != nil {
		t.Fatal(err)
	}
	after, err := p.Pin(t.Context(), "catalog:tree")
	if err != nil || after != before {
		t.Fatal("irrelevant metadata changed pin", after, err)
	}
	writeContent(t, root, "tree/file", "changed", 0o644)
	after, err = p.Pin(t.Context(), "catalog:tree")
	if err != nil || after == before {
		t.Fatal("content edit did not change pin", after, err)
	}
	writeContent(t, root, "tree/added", "added", 0o644)
	added, err := p.Pin(t.Context(), "catalog:tree")
	if err != nil || added == after {
		t.Fatal("addition did not change pin", added, err)
	}
	if err := os.Remove(filepath.Join(root, "tree", "added")); err != nil {
		t.Fatal(err)
	}
	removed, err := p.Pin(t.Context(), "catalog:tree")
	if err != nil || removed != after {
		t.Fatal("removal did not restore pin", removed, err)
	}
}
func TestContentRefusesUnsafeNamesAndMembers(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "safe/file", "hello", 0o644)
	p := localProvider(t, root)
	for _, uri := range []string{"https://example.invalid/x", "catalog:", "catalog:../safe/file", "catalog:/safe/file", "catalog:safe//file", "catalog:safe/./file", "catalog:safe\\file", "catalog:safe:x", "catalog:safe/\nfile", "catalog:safe/\xff", "catalog:e\u0301"} {
		if _, err := p.Pin(t.Context(), uri); !errors.Is(err, ErrContent) {
			t.Errorf("unsafe URI accepted: %q (%v)", uri, err)
		}
	}
	cases := []struct {
		name  string
		setup func(string) error
	}{
		{"selected-link", func(root string) error { return os.Symlink("safe", filepath.Join(root, "target")) }},
		{"member-link", func(root string) error {
			if err := os.Mkdir(filepath.Join(root, "target"), 0o700); err != nil {
				return err
			}
			return os.Symlink("../safe/file", filepath.Join(root, "target", "link"))
		}},
		{"fifo", func(root string) error {
			if err := os.Mkdir(filepath.Join(root, "target"), 0o700); err != nil {
				return err
			}
			return syscall.Mkfifo(filepath.Join(root, "target", "pipe"), 0o600)
		}},
		{"decomposed-empty-dir", func(root string) error { return os.MkdirAll(filepath.Join(root, "target", "e\u0301"), 0o700) }},
		{"case-dirs", func(root string) error {
			if err := os.MkdirAll(filepath.Join(root, "target", "A"), 0o700); err != nil {
				return err
			}
			return os.Mkdir(filepath.Join(root, "target", "a"), 0o700)
		}},
		{"unicode-fold", func(root string) error {
			writeContent(t, root, "target/Σ", "one", 0o644)
			writeContent(t, root, "target/ς", "two", 0o644)
			return nil
		}},
		{"empty-tree", func(root string) error { return os.Mkdir(filepath.Join(root, "target"), 0o700) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeContent(t, root, "safe/file", "hello", 0o644)
			if err := tc.setup(root); err != nil {
				t.Fatal(err)
			}
			p := localProvider(t, root)
			if _, err := p.Pin(t.Context(), "catalog:target"); !errors.Is(err, ErrContent) {
				t.Fatal("unsafe member accepted", err)
			}
		})
	}
	if err := os.Symlink("safe", filepath.Join(root, "ancestor")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadDefinition(t.Context(), "catalog:ancestor/file"); !errors.Is(err, ErrContent) {
		t.Fatal("selected ancestor link accepted", err)
	}
}
func TestAuthorizedRootLinkAndCancellation(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "é", "hello", 0o644)
	link := filepath.Join(t.TempDir(), "authorized-root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	first, err := localProvider(t, root).Pin(t.Context(), "catalog:é")
	if err != nil {
		t.Fatal(err)
	}
	second, err := localProvider(t, link).Pin(t.Context(), "catalog:é")
	if err != nil || first != second {
		t.Fatal(second, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := localProvider(t, root).Pin(ctx, "catalog:é"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLocalDefinitionResolutionVerifiesWholeSkillPackage(t *testing.T) {
	root := t.TempDir()
	writeContent(t, root, "skill/SKILL.md", "instructions", 0o644)
	writeContent(t, root, "skill/support.txt", "support", 0o644)
	provider := localProvider(t, root)
	pin, err := provider.Pin(t.Context(), "catalog:skill")
	if err != nil {
		t.Fatal(err)
	}
	definitions, repo, _ := definitionHarness(t, Policy{})
	definitions.content = provider
	d := minimalDefinition()
	d.Requirements.Skills = []agentdef.Skill{{Name: "example-skill", Content: agentdef.Ref{URI: "catalog:skill", Digest: pin.Digest}}}
	writeContent(t, root, "definition.md", string(authored(t, d)), 0o644)
	indexed, err := definitions.Index(t.Context(), "catalog:definition.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := definitions.Load(t.Context(), indexed.Pin); err != nil {
		t.Fatal(err)
	}
	writeContent(t, root, "skill/support.txt", "changed support", 0o644)
	if _, err := definitions.Load(t.Context(), indexed.Pin); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("unchanged SKILL.md hid support-file mutation", err)
	}
	if _, err := repo.Definition(t.Context(), indexed.Pin.ID, indexed.Pin.Revision); err != nil {
		t.Fatal("verification mutated the index", err)
	}
}

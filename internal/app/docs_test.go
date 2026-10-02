package app

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedDocsProgressiveDisclosureAndConfinement(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	service := (&Service{CatalogRoot: filepath.Join(home, "catalog")}).Docs()
	// A host file matching a document name cannot replace binary content.
	if err := os.WriteFile(filepath.Join(home, "connect.md"), []byte("host canary"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, meta := range service.List() {
		if meta.Title == "" || meta.Trigger == "" || meta.URI == "" {
			t.Fatalf("incomplete metadata: %+v", meta)
		}
		doc, err := service.Get(meta.ID)
		if err != nil || doc.Body == "" {
			t.Fatalf("get %s: %v", meta.ID, err)
		}
		for _, file := range doc.Files {
			got, err := service.GetFile(meta.ID, file.Path)
			if err != nil || got.Body == "" || got.Size != len(got.Body) || got.SHA256 != file.SHA256 {
				t.Fatalf("fetch %s: %+v %v", file.Path, got, err)
			}
		}
	}
	for _, name := range []string{"../connect.md", "/etc/passwd", "file://secret", "docs/agents/mcp/connect.md/../discovery.md", filepath.Join(home, "connect.md")} {
		if _, err := service.GetFile("connect", name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("arbitrary path accepted: %s (%v)", name, err)
		}
	}
	if _, err := service.Get("../connect"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("invalid guide accepted")
	}
}

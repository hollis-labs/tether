package mcpadapter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/catalogcheck"
	"github.com/hollis-labs/tether/internal/client"
)

type catalogContentCapture struct {
	*catalogapply.Engine
	draft []catalogapply.File
}

func (c *catalogContentCapture) ValidateBundle(files []catalogapply.File, opt catalogapply.Options) (catalogcheck.Report, error) {
	c.draft = files
	return c.Engine.ValidateBundle(files, opt)
}

// Exercise MCP argument decoding, daemon transport and the real plan engine.
// Validation observes the bytes actually written into the scratch catalog.
func TestCatalogFileContentBytes(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"terminal_lf", "id: fixture\n", "id: fixture\n"},
		{"surrounding_whitespace", " \t\n# fixture\r\n  id: fixture  \n\n", " \t\n# fixture\r\n  id: fixture  \n\n"},
		{"whitespace_only", " \t\r\n\n", " \t\r\n\n"},
		{"empty", "", ""},
		{"absent", nil, ""},
		{"wrong_type", float64(42), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			var observed []string
			eng := &catalogapply.Engine{Root: root, BackupRoot: t.TempDir(), Validate: func(scratch string) error {
				b, err := os.ReadFile(filepath.Join(scratch, "agents/fixture.yaml"))
				if os.IsNotExist(err) {
					return nil
				} // baseline has no fixture
				if err != nil {
					return err
				}
				observed = append(observed, string(b))
				return nil
			}}
			capture := &catalogContentCapture{Engine: eng}
			srv := httptest.NewServer(api.NewHandler(api.Deps{CatalogApply: capture}))
			defer srv.Close()
			a := &Adapter{token: "test", scopes: map[string]struct{}{ScopeCatalogWrite: {}}, client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))}
			file := map[string]any{"path": "  agents/fixture.yaml \n"}
			if tc.value != nil {
				file["content"] = tc.value
			}
			args := map[string]any{"name": " fixture ", "task_id": " task ", "files": []any{file}}
			result := callHandler(t, a.handleCatalogPlan, args)
			if tc.want == "" {
				if !result.IsError || !strings.Contains(fmt.Sprint(result.StructuredContent), "empty content") {
					t.Fatalf("empty/malformed plan content must still be refused: %+v", result)
				}
				result = callHandler(t, a.handleCatalogValidate, map[string]any{"files": []any{file}})
				if !result.IsError || !strings.Contains(fmt.Sprint(result.StructuredContent), "empty content") {
					t.Fatalf("empty/malformed draft content must still be refused: %+v", result)
				}
				return
			}
			if result.IsError {
				t.Fatalf("plan: %+v", result)
			}
			body := parseToolJSON(t, result)
			id := body["plan_id"].(string)
			plan, ok := eng.Get(id)
			if !ok {
				t.Fatal("plan not stored")
			}
			wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.want)))
			if plan.Name != "fixture" || plan.TaskID != "task" || len(plan.Files) != 1 || plan.Files[0].Path != "agents/fixture.yaml" {
				t.Fatalf("identifier normalization changed: %+v", plan)
			}
			if got := plan.Files[0].NewSHA256; got != wantHash {
				t.Errorf("plan hash = %s, want %s for %q", got, wantHash, tc.want)
			}
			if got := body["files"].([]any)[0].(map[string]any)["new_sha256"]; got != wantHash {
				t.Errorf("MCP hash = %v, want %s", got, wantHash)
			}
			if len(observed) == 0 {
				t.Fatal("plan never validated scratch bytes")
			}
			for _, got := range observed {
				if got != tc.want {
					t.Errorf("plan scratch = %q, want %q", got, tc.want)
				}
			}
			observed = nil
			result = callHandler(t, a.handleCatalogValidate, map[string]any{"files": []any{file}})
			if result.IsError {
				t.Fatalf("validate: %+v", result)
			}
			if len(capture.draft) != 1 || capture.draft[0].Content != tc.want || capture.draft[0].Path != "agents/fixture.yaml" {
				t.Fatalf("draft boundary changed content/path: %+v", capture.draft)
			}

			if _, err := eng.Apply(context.Background(), id); err == nil {
				t.Fatal("unapproved plan applied")
			}
			if _, err := os.Stat(filepath.Join(root, "agents/fixture.yaml")); !os.IsNotExist(err) {
				t.Fatalf("live fixture changed: %v", err)
			}
		})
	}
}

func TestCatalogFileInputGuards(t *testing.T) {
	eng := &catalogapply.Engine{Root: t.TempDir(), BackupRoot: t.TempDir(), Validate: func(string) error { return nil }}
	srv := httptest.NewServer(api.NewHandler(api.Deps{CatalogApply: eng}))
	defer srv.Close()
	a := &Adapter{token: "test", scopes: map[string]struct{}{ScopeCatalogWrite: {}}, client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))}
	for _, files := range []any{
		[]any{"not an object"},
		[]any{map[string]any{"path": "../escape.yaml", "content": "id: fixture\n"}},
		[]any{map[string]any{"path": float64(42), "content": "id: fixture\n"}},
		"not an array",
	} {
		for _, handler := range []struct {
			name string
			h    func(context.Context, map[string]any) (any, error)
		}{{"plan", a.handleCatalogPlan}, {"validate", a.handleCatalogValidate}} {
			// A non-array files value has historically meant no draft for validate.
			if _, ok := files.(string); ok && handler.name == "validate" {
				continue
			}
			result := callHandler(t, handler.h, map[string]any{"name": "fixture", "files": files})
			if !result.IsError {
				t.Errorf("%s accepted malformed files %#v", handler.name, files)
			}
		}
	}
	a.scopes = nil
	result := callHandler(t, a.handleCatalogPlan, map[string]any{"name": "fixture", "files": []any{map[string]any{"path": "agents/fixture.yaml", "content": "id: fixture\n"}}})
	if !result.IsError {
		t.Fatal("plan without catalog.write accepted")
	}
}

func TestCatalogPlanApplyOffTransport(t *testing.T) {
	root := t.TempDir()
	reloads := 0
	eng := &catalogapply.Engine{Root: root, BackupRoot: t.TempDir(), ApprovalOff: true, Validate: func(string) error { return nil }, Reload: func(context.Context) ([]string, error) { reloads++; return nil, nil }}
	srv := httptest.NewServer(api.NewHandler(api.Deps{CatalogApply: eng}))
	defer srv.Close()
	a := &Adapter{token: "test", scopes: map[string]struct{}{ScopeCatalogWrite: {}}, client: client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))}
	result := callHandler(t, a.handleCatalogPlan, map[string]any{"name": "immediate", "apply": true, "files": []any{map[string]any{"path": "launches/dev.yaml", "content": "id: dev\nagent: dev\n"}}})
	if result.IsError {
		t.Fatalf("plan+apply: %+v", result)
	}
	body := parseToolJSON(t, result)
	if body["state"] != "applied" || body["approved_by"] != "approval-disabled" || reloads != 1 {
		t.Fatalf("response: %+v reloads=%d", body, reloads)
	}
	if _, err := os.Stat(filepath.Join(root, "launches/dev.yaml")); err != nil {
		t.Fatal(err)
	}
}

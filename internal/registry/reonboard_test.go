package registry_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestReonboardProjects(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	// 1. Create a temp catalog directory
	catDir := t.TempDir()
	projDir := filepath.Join(catDir, "projects")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	// Write tether.yaml catalog file
	tetherYAML := `
id: tether
name: Tether Control Plane
repo_root: /Users/chrispian/dev/hollis-labs/apps/tether
tracking_root: /Users/chrispian/dev/agent-os/workspaces/execution/tether
`
	if err := os.WriteFile(filepath.Join(projDir, "tether.yaml"), []byte(tetherYAML), 0o644); err != nil {
		t.Fatalf("write tether.yaml: %v", err)
	}

	// 2. Seed a legacy imported row in the database
	legacyMeta, _ := json.Marshal(map[string]any{
		"repo_root":     "/Users/chrispian/dev/hollis-labs/apps/legacy",
		"tracking_root": "/Users/chrispian/dev/agent-os/workspaces/execution/legacy",
	})
	legacyProfile, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName:   "Legacy Project",
		KindMeta:      legacyMeta,
		LastUpdatedBy: "system:bootstrap",
		Callback:      &registry.Callback{Scheme: "file", Target: "file:///some/path.yaml"},
	})
	if err != nil {
		t.Fatalf("seed legacy profile: %v", err)
	}

	// 3. Run ReonboardProjects
	report, err := svc.ReonboardProjects(ctx, catDir)
	if err != nil {
		t.Fatalf("ReonboardProjects: %v", err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("ReonboardProjects reported errors: %v", report.Errors)
	}
	if report.Created != 1 {
		t.Errorf("Created = %d, want 1 (tether)", report.Created)
	}
	if report.Updated != 1 {
		t.Errorf("Updated = %d, want 1 (legacy row)", report.Updated)
	}

	// 4. Verify the newly created Tether row conforms to the new contract
	tetherRow, err := svc.LookupBy(ctx, registry.KindProject, "tether", "tether")
	if err != nil {
		t.Fatalf("lookup tether: %v", err)
	}
	if tetherRow.DisplayName != "Tether Control Plane" {
		t.Errorf("DisplayName = %q, want 'Tether Control Plane'", tetherRow.DisplayName)
	}
	if tetherRow.Props["repo_root"] != "/Users/chrispian/dev/hollis-labs/apps/tether" {
		t.Errorf("Props[repo_root] = %q", tetherRow.Props["repo_root"])
	}
	if tetherRow.Props["tesseract_namespace"] != "user/chrispian/knowledge/tether" {
		t.Errorf("Props[tesseract_namespace] = %q", tetherRow.Props["tesseract_namespace"])
	}
	if tetherRow.LastUpdatedBy != "operator:re-onboard" {
		t.Errorf("LastUpdatedBy = %q, want operator:re-onboard", tetherRow.LastUpdatedBy)
	}
	if tetherRow.FieldMetadata["props"].Class != registry.FieldClassAuthored {
		t.Errorf("FieldMetadata[props].Class = %q, want authored", tetherRow.FieldMetadata["props"].Class)
	}

	// 5. Verify the legacy row was upgraded
	legacyRow, err := svc.Lookup(ctx, legacyProfile.URN)
	if err != nil {
		t.Fatalf("lookup legacy row: %v", err)
	}
	if legacyRow.LastUpdatedBy != "operator:re-onboard" {
		t.Errorf("legacy LastUpdatedBy = %q, want operator:re-onboard", legacyRow.LastUpdatedBy)
	}
	if legacyRow.Props["repo_root"] != "/Users/chrispian/dev/hollis-labs/apps/legacy" {
		t.Errorf("legacy Props[repo_root] = %q", legacyRow.Props["repo_root"])
	}
	if legacyRow.Props["tesseract_namespace"] == "" {
		t.Errorf("legacy Props[tesseract_namespace] is empty")
	}
	if legacyRow.FieldMetadata["props"].Class != registry.FieldClassAuthored {
		t.Errorf("legacy FieldMetadata[props].Class = %q, want authored", legacyRow.FieldMetadata["props"].Class)
	}
}

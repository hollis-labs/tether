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
	if len(legacyRow.KindMeta) != 0 {
		t.Errorf("legacy KindMeta = %s, want nil/empty", string(legacyRow.KindMeta))
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
	// Verify upgradeProjectRow attached substrate="tether" external ID
	legacyByExt, err := svc.LookupBy(ctx, registry.KindProject, "legacy-project", "tether")
	if err != nil {
		t.Fatalf("lookup legacy row by external ID: %v", err)
	}
	if legacyByExt.URN != legacyProfile.URN {
		t.Errorf("legacyByExt.URN = %q, want %q", legacyByExt.URN, legacyProfile.URN)
	}

	// 6. Verify idempotency on second run
	report2, err := svc.ReonboardProjects(ctx, catDir)
	if err != nil {
		t.Fatalf("ReonboardProjects 2nd run: %v", err)
	}
	if report2.Created != 0 {
		t.Errorf("2nd run Created = %d, want 0", report2.Created)
	}
	if report2.Updated != 0 {
		t.Errorf("2nd run Updated = %d, want 0", report2.Updated)
	}

	// 7. Verify updating existing file re-onboards via tether external ID lookup
	tetherYAMLUpdated := `
id: tether
name: Tether Control Plane Updated
repo_root: /Users/chrispian/dev/hollis-labs/apps/tether-v2
`
	if err := os.WriteFile(filepath.Join(projDir, "tether.yaml"), []byte(tetherYAMLUpdated), 0o644); err != nil {
		t.Fatalf("write updated tether.yaml: %v", err)
	}
	report3, err := svc.ReonboardProjects(ctx, catDir)
	if err != nil {
		t.Fatalf("ReonboardProjects 3rd run: %v", err)
	}
	if report3.Created != 0 {
		t.Errorf("3rd run Created = %d, want 0", report3.Created)
	}
	if report3.Updated != 1 {
		t.Errorf("3rd run Updated = %d, want 1", report3.Updated)
	}
	tetherUpdated, err := svc.LookupBy(ctx, registry.KindProject, "tether", "tether")
	if err != nil {
		t.Fatalf("lookup updated tether: %v", err)
	}
	if tetherUpdated.DisplayName != "Tether Control Plane Updated" {
		t.Errorf("DisplayName = %q, want 'Tether Control Plane Updated'", tetherUpdated.DisplayName)
	}
	if tetherUpdated.Props["repo_root"] != "/Users/chrispian/dev/hollis-labs/apps/tether-v2" {
		t.Errorf("Props[repo_root] = %q, want .../tether-v2", tetherUpdated.Props["repo_root"])
	}
}

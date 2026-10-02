package mcpgateway

import (
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMetadataLintReportsWithoutInferringOrRewriting(t *testing.T) {
	for _, tc := range []struct {
		description string
		annotations *mcpsdk.ToolAnnotations
		codes       []string
	}{
		{"run start post a lookup", nil, []string{"missing_annotations"}},
		{strings.Repeat("界", 2048), &mcpsdk.ToolAnnotations{}, nil},
		{strings.Repeat("界", 2049), &mcpsdk.ToolAnnotations{}, []string{"description_length"}},
		{" \nDisabled: unavailable upstream feature", nil, []string{"missing_annotations", "disabled_description"}},
		{"disabled tool", &mcpsdk.ToolAnnotations{}, []string{"disabled_description"}},
		{"Disabledness is not a disabled declaration", &mcpsdk.ToolAnnotations{}, nil},
		{"Read disabled records", &mcpsdk.ToolAnnotations{}, nil},
	} {
		tool := &mcpsdk.Tool{Name: "alpha_lookup", Description: tc.description, Annotations: tc.annotations, InputSchema: map[string]any{"type": "object"}}
		before := *tool
		var codes []string
		for _, finding := range LintTool("alpha", tool) {
			codes = append(codes, finding.Code)
		}
		if !reflect.DeepEqual(codes, tc.codes) || !reflect.DeepEqual(*tool, before) {
			t.Fatalf("lint changed metadata or guessed behavior: codes=%v want=%v tool=%+v", codes, tc.codes, tool)
		}
	}
}

func TestStatusCountsExclusionsAndAvailabilityPerOrigin(t *testing.T) {
	read := &mcpsdk.ToolAnnotations{ReadOnlyHint: true}
	entries := []Entry{
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_read", Annotations: read}},
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_delete", Annotations: read}},
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_write", Annotations: &mcpsdk.ToolAnnotations{}}},
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_other", Annotations: read}},
		{Origin: "beta", Tool: &mcpsdk.Tool{Name: "beta_read", Annotations: read}},
		{Origin: "gamma", Tool: &mcpsdk.Tool{Name: "gamma_read", Annotations: read}},
	}
	snapshot := Snapshot{Entries: entries, Origins: []OriginStatus{
		{ID: "alpha", Status: "connected", ToolCount: 4, InventoryExamined: true, AvailableTools: 4},
		{ID: "beta", Status: "connected", ToolCount: 1, InventoryExamined: true, AvailableTools: 1},
		{ID: "gamma", Status: "failed", ToolCount: 1, InventoryExamined: true, Error: "disconnected"},
	}}
	s := Service{Selection: Selection{Mode: Flat}, Snapshot: func() Snapshot { return snapshot }, Policy: &Policy{Selection: ProfileSelection{ID: "reads", Source: "argument", Profile: &Profile{Servers: []string{"alpha", "gamma"}, ReadOnly: true, Tools: ToolRules{Allow: []string{"*_read"}, Deny: []string{"*delete*"}}}}}}
	status := s.Status("alpha_read")
	excluded := map[string]int{"profile deny": 1, "profile read_only requires readOnlyHint=true": 1, "profile allow": 1, "profile servers": 1}
	if status.CatalogedTools != 6 || status.EligibleTools != 2 || status.AvailableTools != 1 || status.HiddenTools != 4 || status.Complete || !reflect.DeepEqual(status.Exclusions, excluded) {
		t.Fatalf("counts=%+v", status)
	}
	if status.Tool == nil || status.Tool.Origin != "alpha" || status.Tool.Annotations != read || status.Visible == nil || !*status.Visible {
		t.Fatalf("authored metadata=%+v", status)
	}
	if status.Origins[0].EligibleTools != 1 || status.Origins[0].AvailableTools != 1 || status.Origins[0].HiddenTools != 3 || status.Origins[1].AvailableTools != 0 || status.Origins[2].EligibleTools != 1 || status.Origins[2].AvailableTools != 0 {
		t.Fatalf("origin counts=%+v", status.Origins)
	}
	if snapshot.Origins[0].AvailableTools != 4 || snapshot.Origins[0].Exclusions != nil {
		t.Fatal("status mutated source inventory")
	}
	if hidden := s.Status("alpha_delete"); hidden.Tool != nil || hidden.Reason != "profile deny" {
		t.Fatalf("excluded metadata leaked: %+v", hidden)
	}
	snapshot.Origins[2].Status = "connected"
	if status := s.Status("gamma_read"); status.AvailableTools != 2 || !status.Complete {
		t.Fatalf("recovered origin=%+v", status)
	}
}

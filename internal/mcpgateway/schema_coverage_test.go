package mcpgateway

import (
	"encoding/json"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStatusReportsSchemaCoveragePerOriginAndInTotal(t *testing.T) {
	obj := map[string]any{"type": "object"}
	entries := []Entry{
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_a", InputSchema: obj, OutputSchema: obj, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}, Title: "A"}},
		{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_b", InputSchema: obj, Annotations: &mcpsdk.ToolAnnotations{}}},
		{Origin: "beta", Tool: &mcpsdk.Tool{Name: "beta_c", InputSchema: obj}},
		{Origin: "beta", Tool: &mcpsdk.Tool{Name: "beta_d", InputSchema: obj, OutputSchema: obj, Title: "D"}},
	}
	svc := &Service{Snapshot: func() Snapshot {
		return Snapshot{Entries: entries, Origins: []OriginStatus{
			{ID: "alpha", Status: "connected", ToolCount: 2, InventoryExamined: true},
			{ID: "beta", Status: "connected", ToolCount: 2, InventoryExamined: true},
		}}
	}}
	st := svc.Status("")
	if st.OutputSchemaTools != 2 || st.AnnotatedTools != 2 || st.TitledTools != 2 {
		t.Fatalf("totals: output=%d annotated=%d titled=%d", st.OutputSchemaTools, st.AnnotatedTools, st.TitledTools)
	}
	byID := map[string]OriginStatus{}
	for _, o := range st.Origins {
		byID[o.ID] = o
	}
	if a := byID["alpha"]; a.OutputSchemaTools != 1 || a.AnnotatedTools != 2 || a.TitledTools != 1 {
		t.Fatalf("alpha: %+v", a)
	}
	if b := byID["beta"]; b.OutputSchemaTools != 1 || b.AnnotatedTools != 0 || b.TitledTools != 1 {
		t.Fatalf("beta: %+v", b)
	}
	sum := st.Summary()
	if sum.SchemaCoverage != (SchemaCoverage{Tools: 4, WithOutputSchema: 2, WithAnnotations: 2, WithTitle: 2}) {
		t.Fatalf("summary coverage: %+v", sum.SchemaCoverage)
	}
}

func TestCompactStatusKeepsHeadroomUnderTheSizeBudget(t *testing.T) {
	raw, _ := json.Marshal(bigStatus().Summary())
	// The guard in TestStatusSummaryIsCompactAndExplainsIncomplete is 4096; keep
	// some room so the next field does not tip it over unnoticed.
	if len(raw) > 3700 {
		t.Fatalf("compact status is %d bytes; budget 4096 leaves too little headroom above 3700", len(raw))
	}
}

package mcpgateway

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
)

func TestResourcePolicyReadOnlyUIAndGeneralResources(t *testing.T) {
	const uri = "ui://app/mutator.html"
	snapshot := Snapshot{Entries: []Entry{{Origin: "app", Tool: &mcpsdk.Tool{Name: "app_write", Meta: mcpsdk.Meta{"ui": map[string]any{"resourceUri": uri}}}}}}
	policy := Policy{Selection: ProfileSelection{Profile: &Profile{ReadOnly: true}}}
	if policy.ResourceAllowed("app", uri, snapshot) {
		t.Fatal("excluded write tool UI allowed")
	}
	if !policy.ResourceAllowed("app", "file://general", snapshot) {
		t.Fatal("general read denied")
	}
	snapshot.Entries[0].Tool.Annotations = &mcpsdk.ToolAnnotations{ReadOnlyHint: true}
	if !policy.ResourceAllowed("app", uri, snapshot) {
		t.Fatal("eligible tool UI denied")
	}
	policy.Floors = []ProfileSelection{{Profile: &Profile{Servers: []string{}}}}
	if policy.ResourceAllowed("app", uri, snapshot) {
		t.Fatal("origin floor widened")
	}
}

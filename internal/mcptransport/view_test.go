package mcptransport

import (
	"testing"

	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestViewOptions_LaunchProfileIsImmutableAuthorityFloor(t *testing.T) {
	id := "readonly"
	floor := mcpgateway.Profile{ReadOnly: true, Servers: []string{"one"}, Tools: mcpgateway.ToolRules{Deny: []string{"*_private"}}}
	caller := Caller{Policy: mcpgateway.SessionPolicy{SessionID: "s", Servers: []string{"one", "two"}, Profile: &id, LaunchProfile: &floor}.Seal()}
	// A later profile edit and an explicit broader selection must not remove
	// the credential's launch-time rules captured in the sealed snapshot.
	cfg := mcpgateway.Config{Profiles: map[string]mcpgateway.Profile{"readonly": {}, "full": {Servers: []string{"one", "two"}}}}
	opts, err := ViewOptions(caller, cfg, []mcpgateway.Selector{{Value: "full", Source: "request"}}, []mcpgateway.Selector{{Value: "search", Source: "request"}})
	if err != nil {
		t.Fatal(err)
	}
	policy := mcpgateway.Policy{Selection: opts.Profile, Floors: opts.AuthorityProfiles}
	for _, entry := range []mcpgateway.Entry{{Origin: "one", Tool: &mcpsdk.Tool{Name: "one_write"}}, {Origin: "one", Tool: &mcpsdk.Tool{Name: "one_private", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}, {Origin: "two", Tool: &mcpsdk.Tool{Name: "two_read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}} {
		if policy.Exclusion(entry) == "" {
			t.Fatal("request loosened launch floor", entry.Tool.Name)
		}
	}
	read := mcpgateway.Entry{Origin: "one", Tool: &mcpsdk.Tool{Name: "one_read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}
	if reason := policy.Exclusion(read); reason != "" {
		t.Fatal("launch floor over-restricted permitted read", reason)
	}
	if _, err := ViewOptions(caller, cfg, []mcpgateway.Selector{{Value: "missing", Source: "request"}}, nil); err == nil {
		t.Fatal("unknown requested profile accepted")
	}
}

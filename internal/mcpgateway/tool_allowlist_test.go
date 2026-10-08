package mcpgateway

import (
	"encoding/json"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"testing"
)

func TestToolAllowlistSnapshotRoundTrip(t *testing.T) {
	for _, value := range []string{`[]`, `["torque_task_get"]`} {
		profile, err := ParseToolAllowlist(value)
		if err != nil {
			t.Fatal(err)
		}
		policy := SessionPolicy{SessionID: "s", Servers: []string{}, ToolProfile: profile}.Seal()
		raw, err := json.Marshal(policy)
		if err != nil {
			t.Fatal(err)
		}
		var restored SessionPolicy
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if err := restored.Validate(); err != nil {
			t.Fatal(err)
		}
		p := Policy{Selection: ProfileSelection{Profile: restored.ToolProfile}}
		allowed := Entry{Tool: &mcpsdk.Tool{Name: "torque_task_get"}}
		if (p.Exclusion(allowed) == "") != (value != `[]`) {
			t.Fatal("empty allowlist lost deny-all semantics")
		}
		if p.Exclusion(Entry{Tool: &mcpsdk.Tool{Name: "torque_task_update"}}) == "" {
			t.Fatal("unlisted tool reachable")
		}
	}
	for _, value := range []string{"", `null`, `{}`, `[1]`, `["broken["]`} {
		if _, err := ParseToolAllowlist(value); err == nil {
			t.Fatalf("invalid list accepted: %q", value)
		}
	}
}

package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// 'kind' was a free string, and callers sent invented kinds ("message") often
// enough to be a top error class. It is now a real enum in the advertised schema
// (visible to a caller that reads the schema), and a wrong value is still refused
// with the valid kinds named (CW-20261024-0051).
func TestMessageKindIsARealEnumAndABadKindNamesTheValidOnes(t *testing.T) {
	a := newTestAdapterWithDaemon(t)
	s := gomcp.NewServer("t", "0")
	a.registerMessageTools(s)
	c := connectInMemory(t, s)
	ctx := context.Background()

	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"request", "response", "notice", "status_update", "handoff", "escalation"}
	seen := 0
	for _, tool := range tools.Tools {
		if tool.Name != "tether_message_send" && tool.Name != "tether_message_notify" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties struct {
				Kind struct {
					Enum []string `json:"enum"`
				} `json:"kind"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		if strings.Join(schema.Properties.Kind.Enum, ",") != strings.Join(want, ",") {
			t.Fatalf("%s kind enum = %v, want %v", tool.Name, schema.Properties.Kind.Enum, want)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("checked %d tools", seen)
	}

	res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_message_send", Arguments: map[string]any{
		"from": "msg://agent/agent-mux/a", "to": "msg://agent/agent-mux/b", "kind": "message"}})
	if err == nil && !res.IsError {
		t.Fatal("an invented kind must be refused")
	}
	text := ""
	if res != nil {
		text = textOf(res)
	} else {
		text = err.Error()
	}
	for _, k := range want {
		if !strings.Contains(text, k) {
			t.Fatalf("refusal should name the valid kinds; %q missing from %s", k, text)
		}
	}
}

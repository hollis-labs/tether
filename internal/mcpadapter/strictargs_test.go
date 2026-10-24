package mcpadapter

import (
	"context"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMessageToolsRefuseUndeclaredArguments pins the second route into blank
// mail (CW-20261024-0040): a top-level subject/body is not an argument of the
// send tools, was accepted and ignored, and stored payload:null with ok:true.
// Goes through the real registration, because the check wraps the handler there.
func TestMessageToolsRefuseUndeclaredArguments(t *testing.T) {
	base := func(tool string) map[string]any {
		args := map[string]any{
			"from": "msg://agent/agent-mux/sender",
			"to":   "msg://agent/agent-mux/recipient",
			"kind": "notice",
		}
		if tool == "tether_message_notify" {
			args["no_wake"] = true // declared by notify only
		}
		return args
	}
	cases := []struct {
		name    string
		extra   map[string]any
		wantErr []string // substrings of the refusal; nil means the call is accepted
	}{
		{name: "clean call", extra: map[string]any{"payload_json": `{"body":"b"}`}},
		{name: "trace keys are gateway metadata", extra: map[string]any{
			"payload_json": `{"body":"b"}`, "_traceparent": "00-aa-bb-01", "_tracestate": "x=y"}},
		{name: "top-level subject and body", extra: map[string]any{"subject": "s", "body": "b"},
			wantErr: []string{"body", "subject", "payload_json", "nothing was sent"}},
		{name: "top-level body beside a payload_json", extra: map[string]any{"payload_json": `{"x":1}`, "body": "b"},
			wantErr: []string{"body", "payload_json"}},
		{name: "unrelated typo is named, accepted set listed", extra: map[string]any{"thread": "t1"},
			wantErr: []string{"thread", "Accepted:", "thread_id"}},
		{name: "other underscore keys are not an escape hatch", extra: map[string]any{"_sneaky": "x"},
			wantErr: []string{"_sneaky"}},
	}

	for _, tool := range []string{"tether_message_send", "tether_message_notify"} {
		for _, tc := range cases {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				var a *Adapter
				if tool == "tether_message_send" {
					a = newTestAdapterWithDaemon(t)
				} else {
					a, _ = newRecordingNotifyAdapter(t)
				}
				s := gomcp.NewServer("test", "0.0.1")
				a.registerMessageTools(s)
				c := connectInMemory(t, s)

				args := base(tool)
				for k, v := range tc.extra {
					args[k] = v
				}
				res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: args})
				if err != nil {
					t.Fatalf("CallTool: %v", err)
				}
				text := textOf(res)
				if tc.wantErr == nil {
					if res.IsError {
						t.Fatalf("clean call refused: %s", text)
					}
					return
				}
				if !res.IsError {
					t.Fatalf("undeclared argument accepted; result: %s", text)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(text, want) {
						t.Errorf("refusal missing %q:\n%s", want, text)
					}
				}
			})
		}
	}
}

// TestStrictArgsReadsTheToolsOwnSchema fails if a declared argument were ever
// refused: the accepted set must come from the schema, not a second list.
func TestStrictArgsReadsTheToolsOwnSchema(t *testing.T) {
	a := newTestAdapterWithDaemon(t)
	s := gomcp.NewServer("test", "0.0.1")
	a.registerMessageTools(s)
	tool := strictArgs(gomcp.Tool{
		Name:        "probe",
		InputSchema: gomcp.InputSchema(gomcp.StringProp("alpha", "a", true), gomcp.StringProp("beta", "b", false)),
		Handler:     func(context.Context, map[string]any) (any, error) { return "ok", nil },
	})
	if _, err := tool.Handler(context.Background(), map[string]any{"alpha": "1", "beta": "2"}); err != nil {
		t.Fatalf("declared arguments refused: %v", err)
	}
	if _, err := tool.Handler(context.Background(), map[string]any{"alpha": "1", "gamma": "3"}); err == nil ||
		!strings.Contains(err.Error(), "gamma") {
		t.Fatalf("undeclared argument not refused by name: %v", err)
	}
}

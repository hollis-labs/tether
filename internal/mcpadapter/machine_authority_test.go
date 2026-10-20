package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type capturePub struct{ got []events.Event }

func (c *capturePub) Publish(_ context.Context, e events.Event) error {
	c.got = append(c.got, e)
	return nil
}

func TestIsReadOnlyMachineTool(t *testing.T) {
	for tool, want := range map[string]bool{
		"cerberus_list_resources": true, "cerberus_ssh_status": true, "cerberus_get_resource": true,
		"cerberus_ssh_exec": false, "cerberus_enroll_host": false, "list_hosts": true, "cerberus_install": false, "cerberus_get_and_apply": false, "cerberus_find_and_delete": false, "cerberus_check_and_fix": false, "cerberus_query_exec": false, "": false,
	} {
		if got := IsReadOnlyMachineTool("cerberus", tool); got != want {
			t.Errorf("%q = %v, want %v", tool, got, want)
		}
	}
}

func TestMachineAuthorityMiddleware(t *testing.T) {
	cases := []struct {
		name, server, tool, access  string
		dev, wantAllowed, wantAudit bool
	}{
		{"read-only reads", "cerberus", "cerberus_list_resources", "", false, true, true},
		{"read-only denies write", "cerberus", "cerberus_ssh_exec", "read-only", false, false, true},
		{"unknown access fails closed", "cerberus", "cerberus_ssh_exec", "bogus", false, false, true},
		{"read-write allows write", "cerberus", "cerberus_ssh_exec", "read-write", false, true, true},
		{"dev allows write", "cerberus", "cerberus_ssh_exec", "", true, true, true},
		{"other server untouched", "torque", "torque_task_delete", "", false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &capturePub{}
			mw := &MachineAuthorityMiddleware{pub: pub, Access: tc.access, Dev: tc.dev}
			called := false
			ctx := WithServerID(context.Background(), tc.server)
			res, err := mw.Handle(ctx, ToolCall{ToolName: tc.tool}, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) {
				called = true
				return &mcpsdk.CallToolResult{}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if called != tc.wantAllowed || (res.IsError == tc.wantAllowed) {
				t.Fatalf("called=%v isError=%v, want allowed=%v", called, res.IsError, tc.wantAllowed)
			}
			if (len(pub.got) == 1) != tc.wantAudit {
				t.Fatalf("audit events = %d", len(pub.got))
			}
			if tc.wantAudit {
				var d events.MachineAuthorityDecision
				if err := json.Unmarshal([]byte(pub.got[0].PayloadJSON), &d); err != nil || d.Allowed != tc.wantAllowed || pub.got[0].Kind != events.EventTypeMachineAuthorityDecision {
					t.Fatalf("bad audit %+v %v", d, err)
				}
			}
		})
	}
}

func TestIsReadOnlyMachineToolWithAnnotationsAndLastSegment(t *testing.T) {
	yes, no := true, false
	read := &mcpsdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no}
	destroy := &mcpsdk.ToolAnnotations{DestructiveHint: &yes}
	for _, tc := range []struct {
		tool string
		ann  *mcpsdk.ToolAnnotations
		want bool
	}{
		// <noun>_<verb> reads: classified by the last segment, with or without a hint.
		{"cerberus_resource_list", nil, true}, {"cerberus_resource_list", read, true},
		{"cerberus_resource_status", nil, true}, {"cerberus_host_list", nil, true},
		{"cerberus_project_list", nil, true}, {"cerberus_schedule_list", nil, true},
		{"cerberus_host_get", nil, true}, {"cerberus_connector_describe", nil, true},
		// the first segment still works
		{"cerberus_list_resources", nil, true}, {"cerberus_get_resource", nil, true},
		// writes stay writes
		{"cerberus_resource_stop", nil, false}, {"cerberus_docker_destroy", nil, false}, {"cerberus_ssh_exec", nil, false},
		{"cerberus_resource_remove", nil, false}, {"cerberus_schedule_update", nil, false}, {"cerberus_ssh_put", nil, false},
		// a hint can promote an unrecognized name ...
		{"cerberus_resource_logs", read, true}, {"cerberus_host_probe", read, true},
		{"cerberus_resource_logs", nil, false},
		// ... but never overrides a veto: destructiveHint, or a write verb anywhere in the name
		{"cerberus_ssh_get", destroy, false}, {"cerberus_resource_list", destroy, false},
		{"cerberus_ssh_get", &mcpsdk.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &yes}, false},
		{"cerberus_get_and_apply", read, false}, {"cerberus_schedule_dry_run", read, false}, {"cerberus_find_and_delete", read, false},
		{"", read, false},
	} {
		if got := IsReadOnlyMachineToolWith("cerberus", tc.tool, tc.ann); got != tc.want {
			t.Errorf("%s (ann=%+v) = %v, want %v", tc.tool, tc.ann, got, tc.want)
		}
	}
}

// The middleware reads the registered definition's annotations: the 010 acceptance finding.
func TestMachineAuthorityMiddlewareTrustsRegisteredReadOnlyHint(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register("cerberus", nil, []*mcpsdk.Tool{
		{Name: "cerberus_resource_logs", InputSchema: map[string]any{"type": "object"}, Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "cerberus_resource_stop", InputSchema: map[string]any{"type": "object"}},
	}); err != nil {
		t.Fatal(err)
	}
	for tool, wantAllowed := range map[string]bool{"cerberus_resource_logs": true, "cerberus_resource_stop": false, "cerberus_resource_list": true} {
		mw := &MachineAuthorityMiddleware{registry: registry}
		called := false
		_, err := mw.Handle(WithServerID(context.Background(), "cerberus"), ToolCall{ToolName: tool}, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) {
			called = true
			return &mcpsdk.CallToolResult{}, nil
		})
		if err != nil || called != wantAllowed {
			t.Errorf("%s: called=%v err=%v, want allowed=%v", tool, called, err, wantAllowed)
		}
	}
}

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
		"cerberus_list_resources": true, "cerberus_ssh_status": false, "cerberus_get_resource": true,
		"cerberus_ssh_exec": false, "cerberus_enroll_host": false, "list_hosts": true, "cerberus_install": false, "": false,
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

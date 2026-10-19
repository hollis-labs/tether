package events

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/tether/internal/callcontext"
)

func TestCanonicalToolCallPublishedSchemasAndPrivacy(t *testing.T) {
	for _, finished := range []bool{false, true} {
		for _, ok := range []bool{false, true} {
			call := ToolCallEvent{ToolName: "read", Server: "upstream", ArgsSchemaFP: "deadbeef", OK: ok, Error: "SECRET-ARGUMENT-RESULT", ClaimedSessionID: "CLAIM", SessionID: "CLAIM", Attribution: callcontext.Snapshot{TaskID: "FORGED"}, ToolCallDetails: ToolCallDetails{ArgsBytes: 42, ResultBytes: 24, ErrorClass: ToolErrorDenied}}
			ev := CanonicalToolCall(call, finished)
			entry, found := catalog.Lookup(ev.Kind)
			if !found {
				t.Fatal("kind missing from pinned catalog")
			}
			raw, err := catalog.PayloadSchema(entry.PayloadSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema jsonschema.Schema
			if err = json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
				t.Fatal(err)
			}
			if err = resolved.Validate(payload); err != nil {
				t.Fatal(err)
			}
			if ev.SessionID != "" || ev.Scope != ScopeDaemon || strings.Contains(ev.PayloadJSON, "SECRET") || strings.Contains(ev.PayloadJSON, "CLAIM") || strings.Contains(ev.PayloadJSON, "FORGED") {
				t.Fatal("content or claim leaked")
			}
			if finished {
				if payload["ok"] != ok {
					t.Fatal("lost outcome")
				}
			} else {
				payload["ok"] = false
				if resolved.Validate(payload) == nil {
					t.Fatal("pinned started schema must forbid ok")
				}
			}
		}
	}
}

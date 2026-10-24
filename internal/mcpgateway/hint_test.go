package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func schemaOf(required []string, props ...string) map[string]any {
	p := map[string]any{}
	for _, name := range props {
		p[name] = map[string]any{"type": "string"}
	}
	s := map[string]any{"type": "object", "properties": p}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		s["required"] = req
	}
	return s
}

func toolWith(name string, schema any) *mcpsdk.Tool {
	return &mcpsdk.Tool{Name: name, InputSchema: schema}
}

// The shapes below are the argument mistakes that dominate the real error log
// (hub proxy_events since 10-20): the wrong name for a thing the tool wants.
func TestBuildArgHintOnObservedMistakes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tool        *mcpsdk.Tool
		args        map[string]any
		wantDidYou  map[string]string
		wantMissing []string
		wantUnknown []string
	}{
		{
			name:        "torque_comment_add task_id/body for entity_id/content",
			tool:        toolWith("torque_comment_add", schemaOf([]string{"entity_type", "entity_id", "content"}, "entity_type", "entity_id", "content", "author")),
			args:        map[string]any{"task_id": "CW-1", "body": "hello"},
			wantDidYou:  map[string]string{"task_id": "entity_id", "body": "content"},
			wantMissing: []string{"entity_type", "entity_id", "content"},
			wantUnknown: []string{"body", "task_id"},
		},
		{
			name:        "torque_task_get task_id for id",
			tool:        toolWith("torque_task_get", schemaOf([]string{"id"}, "id", "format", "comments")),
			args:        map[string]any{"task_id": "CW-1"},
			wantDidYou:  map[string]string{"task_id": "id"},
			wantMissing: []string{"id"},
			wantUnknown: []string{"task_id"},
		},
		{
			name:        "tesseract_get id for item_id",
			tool:        toolWith("tesseract_get", schemaOf(nil, "domain", "item_id", "key", "namespace")),
			args:        map[string]any{"id": "01M", "budget_tokens": 6000},
			wantDidYou:  map[string]string{"id": "item_id"},
			wantUnknown: []string{"budget_tokens", "id"},
		},
		{
			name:        "memory_write data for payload_data (containment)",
			tool:        toolWith("memory_write", schemaOf([]string{"payload_data"}, "payload_data", "namespace", "key")),
			args:        map[string]any{"data": map[string]any{"a": 1}, "namespace": "n", "key": "k"},
			wantDidYou:  map[string]string{"data": "payload_data"},
			wantMissing: []string{"payload_data"},
			wantUnknown: []string{"data"},
		},
		{
			name:        "a typo (edit distance)",
			tool:        toolWith("recall", schemaOf(nil, "namespace", "query", "limit")),
			args:        map[string]any{"namespcae": "x", "query": "q"},
			wantDidYou:  map[string]string{"namespcae": "namespace"},
			wantUnknown: []string{"namespcae"},
		},
		{
			name:        "tether_message_get with nothing sent",
			tool:        toolWith("tether_message_get", schemaOf([]string{"message_id", "as"}, "message_id", "as")),
			args:        map[string]any{},
			wantMissing: []string{"message_id", "as"},
		},
		{
			name:        "tether_message_send subject/body at top level, from missing",
			tool:        toolWith("tether_message_send", schemaOf([]string{"from", "to", "kind"}, "from", "to", "kind", "payload_json", "thread_id", "in_reply_to")),
			args:        map[string]any{"to": "t", "kind": "notice", "subject": "s", "body": "b"},
			wantMissing: []string{"from"},
			wantUnknown: []string{"body", "subject"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := BuildArgHint(tc.tool, tc.args)
			if hint == nil {
				t.Fatal("want a hint")
			}
			if hint.Tool != tc.tool.Name {
				t.Errorf("tool %q", hint.Tool)
			}
			if !reflect.DeepEqual(hint.DidYouMean, tc.wantDidYou) {
				t.Errorf("did_you_mean = %v, want %v", hint.DidYouMean, tc.wantDidYou)
			}
			if !reflect.DeepEqual(hint.MissingRequired, tc.wantMissing) {
				t.Errorf("missing = %v, want %v", hint.MissingRequired, tc.wantMissing)
			}
			if !reflect.DeepEqual(hint.UnknownArgs, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", hint.UnknownArgs, tc.wantUnknown)
			}
			// Applying the suggestions and filling what is missing must produce a call that
			// fits the schema: that is the "lets a second call succeed" property.
			fixed := map[string]any{}
			for name, v := range tc.args {
				if to, ok := hint.DidYouMean[name]; ok {
					fixed[to] = v
				} else if !contains(hint.UnknownArgs, name) {
					fixed[name] = v
				}
			}
			for _, m := range hint.MissingRequired {
				if _, ok := fixed[m]; !ok {
					fixed[m] = hint.Example[m] // the example carries a value for every required name
				}
			}
			if again := BuildArgHint(tc.tool, fixed); again != nil {
				t.Errorf("corrected call still does not fit the schema: %+v (call %v)", again, fixed)
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestBuildArgHintStaysQuietWhenThereIsNothingToSay(t *testing.T) {
	schema := schemaOf([]string{"a"}, "a", "b")
	if BuildArgHint(toolWith("t", schema), map[string]any{"a": 1, "b": 2}) != nil {
		t.Error("arguments that fit must produce no hint")
	}
	if BuildArgHint(toolWith("t", schema), map[string]any{"a": 1, "_traceparent": "00-x", "_tracestate": "y"}) != nil {
		t.Error("gateway transport metadata is not an unknown argument")
	}
	if BuildArgHint(toolWith("t", map[string]any{"type": "object"}), map[string]any{"x": 1}) != nil {
		t.Error("a schema that declares nothing gives nothing to compare against")
	}
	if BuildArgHint(nil, map[string]any{"x": 1}) != nil {
		t.Error("nil tool")
	}
	open := schemaOf(nil, "a")
	open["additionalProperties"] = true
	if BuildArgHint(toolWith("t", open), map[string]any{"a": 1, "anything": 2}) != nil {
		t.Error("an open schema legitimately accepts extra names")
	}
	// Open schema, but a required argument is still missing: that is reportable.
	open["required"] = []any{"a"}
	if h := BuildArgHint(toolWith("t", open), map[string]any{"anything": 2}); h == nil || len(h.UnknownArgs) != 0 || len(h.MissingRequired) != 1 {
		t.Errorf("open schema with a missing required arg: %+v", h)
	}
}

func TestBuildArgHintIsCompact(t *testing.T) {
	var props []string
	for i := 0; i < 40; i++ {
		props = append(props, fmt.Sprintf("field_%02d", i))
	}
	hint := BuildArgHint(toolWith("big_tool", schemaOf([]string{"field_00"}, props...)), map[string]any{"fieldd_01": 1})
	if hint == nil {
		t.Fatal("want a hint")
	}
	if len(hint.AcceptedArgs) != 0 || !strings.Contains(hint.Next, "tether_tool_list") || !strings.Contains(hint.Next, "big_tool") {
		t.Errorf("a long accepted list must be replaced by a pointer to tether_tool_list: %+v", hint)
	}
	raw, _ := json.Marshal(hint)
	if len(raw) > 600 {
		t.Errorf("hint is %d bytes; it costs tokens and should stay small", len(raw))
	}
	small := BuildArgHint(toolWith("small", schemaOf([]string{"a"}, "a", "b", "c")), map[string]any{"x": 1})
	if !reflect.DeepEqual(small.AcceptedArgs, []string{"a", "b", "c"}) {
		t.Errorf("a short accepted list should be shown: %v", small.AcceptedArgs)
	}
}

func TestSuggestionsUseEachDeclaredNameOnce(t *testing.T) {
	hint := BuildArgHint(toolWith("t", schemaOf([]string{"entity_id"}, "entity_id")), map[string]any{"task_id": 1, "item_id": 2})
	if hint == nil || len(hint.DidYouMean) != 1 {
		t.Fatalf("two unknown names must not both claim the same declared name: %+v", hint)
	}
	// And an empty argument name must not panic.
	if h := BuildArgHint(toolWith("t", schemaOf([]string{"a"}, "a")), map[string]any{"": 1}); h == nil {
		t.Error("an empty name is unknown, not a crash")
	}
}

func TestPlaceholdersFollowTheSchema(t *testing.T) {
	d := declared{
		required: []string{"kind", "n", "ok", "list", "obj", "named", "withdefault", "untyped"},
		props: map[string]map[string]any{
			"kind":        {"type": "string", "enum": []any{"request", "notice"}},
			"n":           {"type": "integer"},
			"ok":          {"type": "boolean"},
			"list":        {"type": "array"},
			"obj":         {"type": "object"},
			"named":       {"type": "string"},
			"withdefault": {"type": "string", "default": "x"},
			"untyped":     nil,
		},
	}
	got := exampleFor(d)
	want := map[string]any{"kind": "request", "n": 0, "ok": false, "list": []any{}, "obj": map[string]any{}, "named": "<named>", "withdefault": "x", "untyped": "<untyped>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("example = %v\nwant     %v", got, want)
	}
	if ex, own := SynthesizedExample(map[string]any{"type": "object", "examples": []any{map[string]any{"a": "real"}}}); !own || ex["a"] != "real" {
		t.Errorf("a schema's own example wins: %v %v", ex, own)
	}
}

func TestServiceCallAppendsTheHintOnlyToFailedResults(t *testing.T) {
	tool := toolWith("alpha_get", schemaOf([]string{"id"}, "id", "format"))
	failed := &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"error":"unknown argument task_id"}`}}}
	ok := &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "fine"}}}
	var next *mcpsdk.CallToolResult
	var dispatchErr error
	svc := &Service{
		Snapshot: func() Snapshot {
			return Snapshot{Entries: []Entry{{Origin: "alpha", Tool: tool}}, Origins: []OriginStatus{{ID: "alpha", Status: "connected"}}}
		},
		Dispatch: func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
			return next, dispatchErr
		},
	}
	ctx := context.Background()

	// Failed call with a wrong argument name: original first, hint second, original not mutated.
	next = failed
	got, err := svc.Call(ctx, "alpha_get", map[string]any{"task_id": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 2 || got.Content[0].(*mcpsdk.TextContent).Text != `{"error":"unknown argument task_id"}` {
		t.Fatalf("upstream content must stay first and intact: %+v", got.Content)
	}
	if len(failed.Content) != 1 {
		t.Fatal("the dispatcher's result was mutated")
	}
	var parsed struct {
		Hint ArgHint `json:"gateway_hint"`
	}
	if err := json.Unmarshal([]byte(got.Content[1].(*mcpsdk.TextContent).Text), &parsed); err != nil {
		t.Fatalf("hint block is not JSON: %v", err)
	}
	if parsed.Hint.DidYouMean["task_id"] != "id" || parsed.Hint.Tool != "alpha_get" || !got.IsError {
		t.Fatalf("hint %+v isError=%v", parsed.Hint, got.IsError)
	}

	// Failed call whose arguments are fine (some other failure): untouched.
	if r, _ := svc.Call(ctx, "alpha_get", map[string]any{"id": "x"}, nil); r != failed {
		t.Error("a failure that is not an argument problem must pass through unchanged")
	}
	// Successful call: untouched, even with a wrong name (that is the silent-drop audit's job, not this one).
	next = ok
	if r, _ := svc.Call(ctx, "alpha_get", map[string]any{"task_id": "x"}, nil); r != ok {
		t.Error("a successful result must never be modified")
	}
	// A dispatch error is passed through as is.
	next, dispatchErr = nil, fmt.Errorf("boom")
	if _, err := svc.Call(ctx, "alpha_get", map[string]any{"task_id": "x"}, nil); err == nil || err.Error() != "boom" {
		t.Errorf("dispatch error: %v", err)
	}
	// Unknown target is still a TargetError and does not dispatch.
	if _, err := svc.Call(ctx, "nope", nil, nil); err == nil {
		t.Error("unknown tool must still be refused")
	}
}

func TestSchemaDetailItemsCarryAnExample(t *testing.T) {
	svc := dietService(2, 1<<20)
	res, err := svc.Search(Request{Query: "read widgets", Detail: "schema"})
	if err != nil || res.Returned == 0 {
		t.Fatal(err)
	}
	for _, item := range res.Items {
		if item.Example["field_0"] != "<field_0>" || item.Example["field_1"] != "<field_1>" {
			t.Fatalf("schema item lacks a required-args example: %v", item.Example)
		}
	}
	summary, _ := svc.Search(Request{Query: "read widgets"})
	for _, item := range summary.Items {
		if item.Example != nil {
			t.Fatal("summary detail must stay small: no example")
		}
	}
}

package mcpgateway

import (
	"context"
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"strings"
	"testing"
)

func TestProfileFilteringBoundaryAndOrdering(t *testing.T) {
	entries := []Entry{
		{Tool: &mcpsdk.Tool{Name: "alpha_read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}, Meta: mcpsdk.Meta{"owner": "alpha"}}, Origin: "alpha"},
		{Tool: &mcpsdk.Tool{Name: "alpha_write", Annotations: &mcpsdk.ToolAnnotations{}}, Origin: "alpha"},
		{Tool: &mcpsdk.Tool{Name: "alpha_missing"}, Origin: "alpha"},
		{Tool: &mcpsdk.Tool{Name: "beta_read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, Origin: "beta"},
		{Tool: &mcpsdk.Tool{Name: "tether_health", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}, Origin: "tether"},
	}
	snapshot := Snapshot{Entries: entries, Origins: []OriginStatus{{ID: "alpha", Status: "connected"}, {ID: "beta", Status: "connected"}, {ID: "tether", Status: "connected"}}}
	profile := &Profile{Servers: []string{"beta", "alpha", "tether"}, Tools: ToolRules{Allow: []string{"*_read", "*_write", "*_missing", "tether_*"}, Deny: []string{"beta_*"}}, ReadOnly: true, Order: []string{"tether_health", "alpha_write"}, AlwaysLoad: []string{"alpha_read", "alpha_write"}}
	policy := &Policy{Selection: ProfileSelection{ID: "readers", Profile: profile}, ServerOrder: profile.Servers}
	if err := policy.ValidateNames(snapshot); err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	scored := map[string]bool{}
	service := &Service{Selection: Selection{Mode: Search}, Policy: policy, Snapshot: func() Snapshot { return snapshot }, Score: func(_ string, e Entry) int { scored[e.Tool.Name] = true; return 1 }, Dispatch: func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
		dispatches++
		return &mcpsdk.CallToolResult{}, nil
	}}
	result, err := service.List(Request{})
	if err != nil || len(result.Items) != 2 || result.Items[0].Name != "tether_health" || result.Items[1].Name != "alpha_read" {
		t.Fatalf("list=%+v %v", result, err)
	}
	if result.Items[1].Meta["anthropic/alwaysLoad"] != true || result.Items[1].Meta["owner"] != "alpha" || entries[0].Tool.Meta["anthropic/alwaysLoad"] != nil {
		t.Fatal("hint lost upstream metadata or mutated authored tool")
	}
	if _, err := service.Search(Request{Query: "read"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha_write", "alpha_missing", "beta_read"} {
		if scored[name] {
			t.Fatalf("filtered target ranked: %s", name)
		}
		out, err := service.List(Request{Names: []string{name}})
		if err != nil || out.Items[0].Error == "" || out.Items[0].InputSchema != nil {
			t.Fatalf("hydrated excluded %s", name)
		}
		if _, err := service.Call(context.Background(), name, nil, nil); err == nil {
			t.Fatalf("dispatched excluded %s", name)
		}
	}
	if dispatches != 0 {
		t.Fatal("excluded dispatch reached transport")
	}
	if policy.Exclusion(entries[3]) != "profile deny" {
		t.Fatal("deny lost precedence")
	}
	profile.Tools.Allow = []string{}
	if result, err := service.List(Request{}); err != nil || len(result.Items) != 0 {
		t.Fatal("explicit empty allow widened inventory")
	}
}

func TestProfileSelectionAndValidation(t *testing.T) {
	config := Config{Profiles: map[string]Profile{"reader": {DiscoveryMode: ptr("search")}, "writer": {}}}
	selection, err := ResolveProfile(config, ProfileInputs{Explicit: []Selector{{"reader", "argument"}}, Environment: ptr("writer")})
	if err != nil || selection.ID != "reader" || selection.Source != "argument" {
		t.Fatalf("selection=%+v %v", selection, err)
	}
	for _, in := range []ProfileInputs{{Explicit: []Selector{{"", "argument"}}}, {Environment: ptr("bogus")}, {Explicit: []Selector{{"reader", "a"}, {"writer", "b"}}}} {
		if _, err := ResolveProfile(config, in); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	for _, profile := range []Profile{{Instructions: strings.Repeat("é", 2049)}, {Tools: ToolRules{Allow: []string{"["}}}, {DiscoveryMode: ptr("directory")}, {Servers: []string{""}}} {
		if profile.Validate() == nil {
			t.Fatalf("invalid profile accepted: %+v", profile)
		}
	}
	if err := (Profile{Instructions: strings.Repeat("é", 2048)}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProfileOriginsIntersectAndFailClosed(t *testing.T) {
	known := map[string]bool{"tether": true, "alpha": true, "beta": true, "disabled": false}
	for _, tc := range []struct {
		origins, restriction []string
		want                 int
	}{
		{nil, []string{"alpha", "beta"}, 2}, {[]string{}, []string{"alpha"}, 0}, {[]string{"tether"}, []string{"alpha"}, 0}, {[]string{"beta", "alpha", "tether"}, []string{"alpha"}, 1},
	} {
		got, err := SelectOrigins(known, tc.restriction, &Profile{Servers: tc.origins})
		if err != nil || len(got) != tc.want {
			t.Fatalf("selected=%v %v", got, err)
		}
	}
	for _, bad := range []string{"bogus", "disabled"} {
		if _, err := SelectOrigins(known, nil, &Profile{Servers: []string{bad}}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestProfileCursorBindingAndPinValidation(t *testing.T) {
	snapshot := Snapshot{Entries: []Entry{{Tool: &mcpsdk.Tool{Name: "a_read"}, Origin: "alpha"}, {Tool: &mcpsdk.Tool{Name: "b_read"}, Origin: "alpha"}}, Origins: []OriginStatus{{ID: "alpha", Status: "connected"}}}
	policy := &Policy{Selection: ProfileSelection{ID: "one", Profile: &Profile{Order: []string{"a_read"}, Tools: ToolRules{Deny: []string{"a_read"}}}}}
	if err := policy.ValidateNames(snapshot); err != nil {
		t.Fatalf("excluded pin should not be unknown: %v", err)
	}
	policy.Selection.Profile.Order = []string{"typo"}
	if err := policy.ValidateNames(snapshot); err == nil {
		t.Fatal("unknown pin accepted")
	}
	policy.Selection.Profile = &Profile{}
	service := &Service{Policy: policy, Snapshot: func() Snapshot { return snapshot }}
	first, err := service.List(Request{Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first=%+v %v", first, err)
	}
	policy.Selection.ID = "two"
	if _, err := service.List(Request{Limit: 1, Cursor: first.NextCursor}); err == nil {
		t.Fatal("cross-profile cursor accepted")
	}
}

func TestProtocolProfilePaginationAvailabilityAndBinding(t *testing.T) {
	entries := []Entry{}
	tools := []*mcpsdk.Tool{}
	for i := 0; i < mcpsdk.DefaultPageSize+5; i++ {
		tool := &mcpsdk.Tool{Name: fmt.Sprintf("alpha_%04d", i), InputSchema: map[string]any{}}
		entries = append(entries, Entry{Tool: tool, Origin: "alpha"})
		tools = append(tools, tool)
	}
	tool := &mcpsdk.Tool{Name: "tether_gateway_status", InputSchema: map[string]any{}}
	tools = append(tools, tool)
	snapshot := Snapshot{Entries: entries, Origins: []OriginStatus{{ID: "alpha", Status: "connected"}}}
	policy := &Policy{Selection: ProfileSelection{ID: "one", Profile: &Profile{Order: []string{"alpha_1004"}}}, ServerOrder: []string{"alpha", "tether"}}
	service := &Service{Selection: Selection{Mode: Flat}, Policy: policy, Snapshot: func() Snapshot { return snapshot }}
	infrastructure := func(name string) bool { return name == tool.Name }
	first, cursor, err := service.ProtocolList(tools, infrastructure, "")
	if err != nil || len(first) != mcpsdk.DefaultPageSize || first[0].Name != "alpha_1004" || cursor == "" {
		t.Fatalf("first page len=%d cursor=%q err=%v", len(first), cursor, err)
	}
	second, next, err := service.ProtocolList(tools, infrastructure, cursor)
	if err != nil || len(second) != 6 || next != "" {
		t.Fatalf("second page len=%d err=%v", len(second), err)
	}
	policy.Selection.ID = "two"
	if _, _, err := service.ProtocolList(tools, infrastructure, cursor); err == nil {
		t.Fatal("cross-profile protocol cursor accepted")
	}
	snapshot.Origins[0].Status = "unavailable"
	listed, _, err := service.ProtocolList(tools, infrastructure, "")
	if err != nil || len(listed) != 1 || listed[0].Name != tool.Name {
		t.Fatalf("unavailable schema leaked: len=%d err=%v", len(listed), err)
	}
}

func TestToolGlobsIncludeSlashAcrossPolicyPaths(t *testing.T) {
	for _, pattern := range []string{"*delete*", "ns/?elete_all", "ns/[d-f]elete_all", "ns/[^a-c]elete_all"} {
		yes, err := matchToolGlob(pattern, "ns/delete_all")
		if err != nil || !yes {
			t.Fatalf("%q did not match slash name: %v", pattern, err)
		}
	}
	entry := Entry{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "ns/delete_all"}}
	policy := &Policy{Selection: ProfileSelection{Profile: &Profile{Tools: ToolRules{Allow: []string{"*"}, Deny: []string{"*delete*"}}}}}
	snapshot := Snapshot{Entries: []Entry{entry}, Origins: []OriginStatus{{ID: "alpha", Status: "connected"}}}
	service := &Service{Policy: policy, Snapshot: func() Snapshot { return snapshot }, Score: func(string, Entry) int { t.Fatal("excluded tool ranked"); return 1 }, Dispatch: func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
		t.Fatal("excluded tool dispatched")
		return nil, nil
	}}
	if out, err := service.Search(Request{Query: "delete"}); err != nil || len(out.Items) != 0 {
		t.Fatalf("search=%v %v", out, err)
	}
	if out, err := service.List(Request{Names: []string{entry.Tool.Name}}); err != nil || out.Items[0].Error == "" {
		t.Fatalf("hydrate=%v %v", out, err)
	}
	if _, err := service.Call(context.Background(), entry.Tool.Name, nil, nil); err == nil {
		t.Fatal("deny bypass")
	}
	if _, err := service.List(Request{Servers: []string{"alpha"}}); err == nil {
		t.Fatal("fully excluded server accepted")
	}
	policy.Selection.Profile.Tools.Deny = nil
	policy.Selection.Profile.Tools.Allow = []string{"*delete*"}
	if out, err := service.List(Request{}); err != nil || len(out.Items) != 1 {
		t.Fatalf("allow slash=%v %v", out, err)
	}
}

func TestSelectedProfileValidationAndUnavailablePins(t *testing.T) {
	config := Config{Profiles: map[string]Profile{"bad": {Tools: ToolRules{Deny: []string{"["}}}, "good": {}}}
	for _, in := range []ProfileInputs{{}, {Explicit: []Selector{{"good", "argument"}}}} {
		if _, err := ResolveProfile(config, in); err != nil {
			t.Fatalf("unselected malformed profile blocked start: %v", err)
		}
	}
	if _, err := ResolveProfile(config, ProfileInputs{Explicit: []Selector{{"bad", "argument"}}}); err == nil {
		t.Fatal("selected malformed profile accepted")
	}
	policy := Policy{Selection: ProfileSelection{Profile: &Profile{Order: []string{"alpha_read"}}}}
	snapshot := Snapshot{Origins: []OriginStatus{{ID: "alpha", Status: "disconnected"}}}
	if err := policy.ValidateNames(snapshot); err != nil {
		t.Fatal(err)
	}
	if warnings := policy.NameWarnings(snapshot); len(warnings) != 1 || !strings.Contains(warnings[0], "upstream alpha unavailable") {
		t.Fatalf("warnings=%v", warnings)
	}
	snapshot.Origins[0].Status = "connected"
	if err := policy.ValidateNames(snapshot); err == nil {
		t.Fatal("connected missing pin accepted")
	}
	for _, name := range []string{"tether_gateway_status", "tether_tool_search", "tether_tool_list", "tether_tool_call"} {
		policy.Selection.Profile.Order = []string{name}
		snapshot.Entries = []Entry{{Tool: &mcpsdk.Tool{Name: name}}}
		if err := policy.ValidateNames(snapshot); err == nil {
			t.Fatal("infrastructure pin accepted")
		}
	}
}

func TestToolGlobSlashQuestionEscapeAndUnicode(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"?", "/", true}, {"*", "a/b\nc", true}, {"[/]", "/", true},
		{`\*`, "*", true}, {`[\-]`, "-", true}, {"[é-ê]?", "é/", true},
		{"*delete*", "ns/read_all", false}, {"DELETE*", "delete", false},
	} {
		got, err := matchToolGlob(tc.pattern, tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("glob %q name %q=%v %v", tc.pattern, tc.name, got, err)
		}
	}
}

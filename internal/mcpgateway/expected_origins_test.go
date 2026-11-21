package mcpgateway

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"reflect"
	"testing"
)

func TestStatusExpectedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, status                                   string
		absent, profile, floor, tools, readOnly, empty bool
		want                                           bool
	}{
		{name: "connected", status: "connected", want: true},
		{name: "absent", absent: true},
		{name: "failed", status: "failed"},
		{name: "reconnecting", status: "reconnecting"},
		{name: "profile hidden", status: "connected", profile: true},
		{name: "grant floor hidden", status: "connected", floor: true},
		{name: "all tools hidden", status: "connected", tools: true},
		{name: "read-only hides every tool", status: "connected", readOnly: true},
		{name: "successful empty inventory", status: "connected", empty: true, want: true},
		{name: "empty inventory under read-only", status: "connected", empty: true, readOnly: true, want: true},
		{name: "empty inventory hidden by profile", status: "connected", empty: true, profile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := Snapshot{ExpectedOrigins: []string{"registry"}, External: []ExternalOrigin{{ID: "ops", Status: "external", ServedAt: "standalone gateway"}}}
			if !tc.absent {
				snapshot.Origins = []OriginStatus{{ID: "registry", Status: tc.status, ToolCount: 1}}
				snapshot.Entries = []Entry{{Origin: "registry", Tool: &mcpsdk.Tool{Name: "registry_read", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false}}}}
			}
			if tc.empty {
				snapshot.Origins[0].ToolCount = 0
				snapshot.Origins[0].InventoryExamined = true
				snapshot.Entries = nil
			}
			svc := Service{Snapshot: func() Snapshot { return snapshot }}
			if tc.profile {
				svc.Policy = &Policy{Selection: ProfileSelection{Profile: &Profile{Servers: []string{"tether"}}}}
			}
			if tc.floor {
				svc.Policy = &Policy{Floors: []ProfileSelection{{Profile: &Profile{Servers: []string{"tether"}}}}}
			}
			if tc.tools {
				svc.Policy = &Policy{Selection: ProfileSelection{Profile: &Profile{Tools: ToolRules{Deny: []string{"*"}}}}}
			}
			if tc.readOnly {
				svc.Policy = &Policy{Selection: ProfileSelection{Profile: &Profile{ReadOnly: true}}}
			}
			st := svc.Status("")
			if st.Complete != tc.want {
				t.Fatalf("complete=%v: %+v", st.Complete, st)
			}
			want := []string{}
			if !tc.want {
				want = append(want, "registry")
			}
			if !reflect.DeepEqual(st.MissingExpected, want) {
				t.Fatalf("missing=%v want=%v", st.MissingExpected, want)
			}
			summary := st.Summary()
			page, err := st.Page(10, "")
			if err != nil || summary.Complete != st.Complete || page.Complete != st.Complete || !reflect.DeepEqual(summary.MissingExpected, want) || !reflect.DeepEqual(page.MissingExpected, want) || len(summary.External) != 1 {
				t.Fatalf("lost expected/external metadata: %+v %+v %v", summary, page, err)
			}
		})
	}
}

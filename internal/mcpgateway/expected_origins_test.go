package mcpgateway

import (
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"reflect"
	"testing"
)

func TestStatusExpectedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, status                  string
		absent, profile, floor, tools bool
		want                          bool
	}{
		{name: "connected", status: "connected", want: true},
		{name: "absent", absent: true},
		{name: "failed", status: "failed"},
		{name: "reconnecting", status: "reconnecting"},
		{name: "profile hidden", status: "connected", profile: true},
		{name: "grant floor hidden", status: "connected", floor: true},
		{name: "all tools hidden", status: "connected", tools: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := Snapshot{ExpectedOrigins: []string{"registry"}, External: []ExternalOrigin{{ID: "ops", Status: "external", ServedAt: "standalone gateway"}}}
			if !tc.absent {
				snapshot.Origins = []OriginStatus{{ID: "registry", Status: tc.status, ToolCount: 1}}
				snapshot.Entries = []Entry{{Origin: "registry", Tool: &mcpsdk.Tool{Name: "registry_read"}}}
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
			if err != nil || !reflect.DeepEqual(summary.MissingExpected, want) || !reflect.DeepEqual(page.MissingExpected, want) || len(summary.External) != 1 {
				t.Fatalf("lost expected/external metadata: %+v %+v %v", summary, page, err)
			}
		})
	}
}

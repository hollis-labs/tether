package launch

import (
	"reflect"
	"testing"
)

func TestCorrelationSealsInheritedAndDuplicateClaims(t *testing.T) {
	for _, c := range []Correlation{{TaskID: "CW-20261012-0062", WorkstreamID: "WS-1", ProjectID: "PRJ-1"}, {}} {
		input := map[string]string{"TETHER_TASK_ID": "wrong", "TETHER_PROJECT_ID": "wrong", "TETHER_WORKSTREAM_ID": "wrong", "KEEP": "yes"}
		sealed := c.SealMap(input)
		if input["TETHER_TASK_ID"] != "wrong" {
			t.Fatal("mutated source")
		}
		if got := CorrelationFromEnv(sealed); got != c {
			t.Fatalf("map correlation=%+v want=%+v", got, c)
		}
		env := c.SealEnv([]string{"KEEP=yes", "TETHER_TASK_ID=ambient", "TETHER_TASK_ID=provider", "TETHER_PROJECT_ID=ambient", "TETHER_WORKSTREAM_ID=ambient"})
		want := []string{"KEEP=yes"}
		if c.TaskID != "" {
			want = append(want, "TETHER_TASK_ID="+c.TaskID, "TETHER_WORKSTREAM_ID="+c.WorkstreamID, "TETHER_PROJECT_ID="+c.ProjectID)
		}
		if !reflect.DeepEqual(env, want) {
			t.Fatalf("env=%v want=%v", env, want)
		}
	}
}

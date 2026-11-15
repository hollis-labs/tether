package execpolicy

import (
	"errors"
	"testing"
)

func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestAdmit(t *testing.T) {
	u := Usage{Total: 2, ByProject: map[string]int{"a": 2}}
	cases := []struct {
		name string
		p    Policy
		want string
	}{
		{"zero policy admits", Policy{}, ""},
		{"paused", Policy{Paused: true, Reason: "maintenance"}, CodePaused},
		{"environment cap full", Policy{MaxSessions: 2}, CodeEnvironmentCapFull},
		{"environment cap has room", Policy{MaxSessions: 3}, ""},
		{"project cap full", Policy{ProjectCaps: map[string]int{"a": 2}}, CodeProjectCapFull},
		{"other project unaffected", Policy{ProjectCaps: map[string]int{"b": 1}}, ""},
		{"paused wins over caps", Policy{Paused: true, MaxSessions: 1}, CodePaused},
	}
	for _, c := range cases {
		if got := code(Admit(c.p, u, "a")); got != c.want {
			t.Errorf("%s: code %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateAndNormalize(t *testing.T) {
	if (Policy{MaxSessions: -1}).Validate() == nil {
		t.Error("negative max_sessions accepted")
	}
	if (Policy{ProjectCaps: map[string]int{"a": -1}}).Validate() == nil {
		t.Error("negative project cap accepted")
	}
	n := Policy{Reason: "stale", ProjectCaps: map[string]int{"a": 0}}.Normalize()
	if n.Reason != "" || n.ProjectCaps != nil {
		t.Errorf("normalize kept %+v", n)
	}
}

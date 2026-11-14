package config

import (
	"strings"
	"testing"
)

func TestValidateHandles(t *testing.T) {
	cases := []struct {
		agents map[string]Agent
		err    string
	}{
		{map[string]Agent{"cmd-builder-1": {ID: "cmd-builder-1", Handle: "builder-1"}, "archie": {ID: "archie"}}, ""},
		{map[string]Agent{"a": {ID: "a", Handle: "dup"}, "b": {ID: "b", Handle: "dup"}}, "both have handle"},
		{map[string]Agent{"a": {ID: "a", Handle: "b"}, "b": {ID: "b"}}, "another agent's id"},
		{map[string]Agent{"a": {ID: "a", Handle: "a"}}, ""},
		{map[string]Agent{"chrispian": {ID: "chrispian"}}, "reserved for the operator"},
		{map[string]Agent{"chrispian": {ID: "chrispian", Handle: "chris-bot"}}, ""},
	}
	for i, tc := range cases {
		err := (&Catalog{Agents: tc.agents}).validateHandles()
		if (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("case %d: err = %v, want %q", i, err, tc.err)
		}
	}
	for h, ok := range map[string]bool{"builder-1": true, "@builder-1": false, "Builder": false, "1x": false, "chrispian": false} {
		if validHandleName(h) != ok {
			t.Errorf("validHandleName(%q) = %v", h, !ok)
		}
	}
}

package mcpgateway

import "testing"

func ptr(value string) *string { return &value }
func TestModePrecedenceAndValidation(t *testing.T) {
	tiers := ModeInputs{Explicit: []Selector{{"flat", "argument"}}, Environment: ptr("search"), Profile: &Profile{ptr("flat")}, Gateway: ptr("search"), Setting: ptr("flat")}
	for _, want := range []Selection{{Flat, "argument"}, {Search, "environment"}, {Flat, "profile"}, {Search, "gateway_config"}, {Flat, "tether_setting"}, {Flat, "default"}} {
		got, err := ResolveMode(tiers)
		if err != nil || got != want {
			t.Fatalf("resolve = %+v, %v; want %+v", got, err, want)
		}
		switch want.Source {
		case "argument":
			tiers.Explicit = nil
		case "environment":
			tiers.Environment = nil
		case "profile":
			tiers.Profile = nil
		case "gateway_config":
			tiers.Gateway = nil
		case "tether_setting":
			tiers.Setting = nil
		}
	}
	for _, bad := range []string{"", "directory", "FLAT", " flat "} {
		for _, inputs := range []ModeInputs{
			{Explicit: []Selector{{bad, "argument"}}},
			{Explicit: []Selector{{"flat", "argument"}}, Environment: ptr(bad)},
			{Explicit: []Selector{{"flat", "argument"}}, Profile: &Profile{ptr(bad)}},
			{Explicit: []Selector{{"flat", "argument"}}, Gateway: ptr(bad)},
			{Explicit: []Selector{{"flat", "argument"}}, Setting: ptr(bad)},
		} {
			if _, err := ResolveMode(inputs); err == nil {
				t.Fatalf("overridden invalid mode %q accepted: %+v", bad, inputs)
			}
		}
	}
	if _, err := ResolveMode(ModeInputs{Explicit: []Selector{{"flat", "query"}, {"search", "header"}}}); err == nil {
		t.Fatal("conflicting same-tier selectors accepted")
	}
	if _, err := ResolveMode(ModeInputs{Explicit: []Selector{{"search", "query"}, {"search", "header"}}}); err != nil {
		t.Fatal(err)
	}
}

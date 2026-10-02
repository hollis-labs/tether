package launchprofile

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestResolveRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      *Route
		want    *Route
		invalid bool
	}{
		{name: "absent"},
		{name: "defaults", in: &Route{Channel: "ops"}, want: &Route{Channel: "ops", Kinds: []string{"final", "question", "approval", "failure"}}},
		{name: "selected", in: &Route{Channel: "Ops-1.a_b", Kinds: []string{"question"}}, want: &Route{Channel: "Ops-1.a_b", Kinds: []string{"question"}}},
		{name: "empty kinds", in: &Route{Channel: "ops", Kinds: []string{}}, want: &Route{Channel: "ops", Kinds: []string{}}},
		{name: "unknown kind", in: &Route{Channel: "ops", Kinds: []string{"terminal"}}, invalid: true},
		{name: "empty channel", in: &Route{}, invalid: true},
		{name: "path channel", in: &Route{Channel: "../ops"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRoute(tc.in)
			var invalid *InvalidRouteError
			if tc.invalid {
				if !errors.As(err, &invalid) {
					t.Fatalf("want typed error, got %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip *Route
			if err := json.Unmarshal(raw, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(roundtrip, tc.want) {
				t.Fatalf("JSON lost route semantics: %s", raw)
			}
		})
	}
}

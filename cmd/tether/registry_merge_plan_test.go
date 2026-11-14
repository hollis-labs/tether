package main

import (
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestMergeExternalIDPlan(t *testing.T) {
	src := registry.Profile{ExternalIDs: []registry.ExternalID{{Substrate: "tether", ExternalID: "chief"}, {Substrate: "handle", ExternalID: "chief"}, {Substrate: "cerberus", ExternalID: "c1"}}}
	dst := registry.Profile{ExternalIDs: []registry.ExternalID{{Substrate: "cerberus", ExternalID: "c2"}}}
	move, keep := mergeExternalIDPlan(src, dst)
	if !reflect.DeepEqual(move, []string{"tether=chief", "handle=chief"}) || !reflect.DeepEqual(keep, []string{"cerberus=c1"}) {
		t.Fatalf("move=%v keep=%v", move, keep)
	}
}

package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func TestLaunchRouteFlags(t *testing.T) {
	oldRoute, oldKinds := launchRoute, launchRouteKinds
	defer func() { launchRoute, launchRouteKinds = oldRoute, oldKinds }()
	for _, tc := range []struct {
		name    string
		args    []string
		kinds   []string
		invalid bool
	}{
		{name: "default"},
		{name: "channel", args: []string{"--route", "ops"}, kinds: []string{"final", "question", "approval", "failure"}},
		{name: "selected", args: []string{"--route", "ops", "--route-kinds", "question,approval"}, kinds: []string{"question", "approval"}},
		{name: "empty", args: []string{"--route", "ops", "--route-kinds", ""}, kinds: []string{}},
		{name: "unknown kind", args: []string{"--route", "ops", "--route-kinds", "terminal"}, invalid: true},
		{name: "without channel", args: []string{"--route-kinds", "final"}, invalid: true},
		{name: "empty channel", args: []string{"--route", ""}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().StringVar(&launchRoute, "route", "", "")
			cmd.Flags().StringSliceVar(&launchRouteKinds, "route-kinds", nil, "")
			if err := cmd.Flags().Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			got, err := launchRouteFromFlags(cmd)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid flags accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.args == nil {
				if got != nil {
					t.Fatalf("default routes: %+v", got)
				}
				return
			}
			if got.Channel != "ops" || !reflect.DeepEqual(got.Kinds, tc.kinds) {
				t.Fatalf("route %+v; want kinds %v", got, tc.kinds)
			}
		})
	}
}

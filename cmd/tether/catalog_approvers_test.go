package main

import "testing"

func TestCatalogApproverCommandSurface(t *testing.T) {
	for _, args := range [][]string{{"approvers", "list"}, {"approvers", "show"}, {"breakglass"}, {"breakglass", "end"}} {
		cmd, remaining, err := catalogCmd.Find(args)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("%v: command unavailable (%v %v)", args, remaining, err)
		}
		if cmd.Flags().Lookup("passphrase") != nil {
			t.Fatal("passphrase must never be a CLI argument")
		}
	}
	for _, name := range []string{"principal", "reason", "for"} {
		cmd, _, _ := catalogCmd.Find([]string{"breakglass"})
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

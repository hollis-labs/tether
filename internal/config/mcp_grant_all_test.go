package config

import (
	"errors"
	"slices"
	"testing"
)

func TestExpandMCPGrantAll(t *testing.T) {
	c := &Catalog{MCPServerEnabled: map[string]bool{"torque": true, "tesseract": true, "cerberus": true, "old": false}}
	if got := c.ExpandMCPGrant([]string{"torque"}, nil); !slices.Equal(got, []string{"torque"}) {
		t.Fatalf("no wildcard changed: %v", got)
	}
	if got := c.ExpandMCPGrant([]string{"*"}, nil); !slices.Equal(got, []string{"cerberus", "tesseract", "torque"}) {
		t.Fatalf("* = %v", got)
	}
	noMachine := func(id string) bool { return id == "cerberus" }
	if got := c.ExpandMCPGrant([]string{"tesseract", "*"}, noMachine); !slices.Equal(got, []string{"tesseract", "torque"}) {
		t.Fatalf("* without machine authority = %v", got)
	}
	if err := c.ValidateMCPGrant("agent x", []string{"*"}); err != nil {
		t.Fatalf("* refused in a definition: %v", err)
	}
	if err := c.ValidateMCPGrantEnv("effective", "torque,*"); !errors.Is(err, ErrInvalidMCPGrant) {
		t.Fatalf("an unexpanded * in an effective grant was accepted: %v", err)
	}
}

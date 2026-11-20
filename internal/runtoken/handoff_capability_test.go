package runtoken

import (
	"errors"
	"strings"
	"testing"
)

func TestUnsupportedHandoffKeepsLegacyAndRejectsSuppliedToken(t *testing.T) {
	legacy := []string{"TETHER_RUN_ID=42", IDEnv + "=execution"}
	got, release, err := prepareEnvironment(legacy, "s", false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if strings.Join(got, "\n") != strings.Join(legacy, "\n") {
		t.Fatal("unsupported handoff broke legacy environment")
	}
	if _, _, err := prepareEnvironment(append(legacy, TokenEnv+"=explicit-token"), "s", false); !errors.Is(err, ErrHandoff) {
		t.Fatal("supplied token silently downgraded")
	}
	sealed := SealMap(map[string]string{TokenEnv: "provider-token", IDEnv: "provider-id", "TETHER_RUN_ID": "42"}, map[string]string{IDEnv: "fresh"})
	if sealed[TokenEnv] != "" || sealed[IDEnv] != "fresh" || sealed["TETHER_RUN_ID"] != "42" {
		t.Fatal("unsupported launch retained provider execution authority")
	}
}

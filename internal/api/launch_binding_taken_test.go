package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLaunchResponseCarriesBindingTakenFrom(t *testing.T) {
	b, _ := json.Marshal(launchResponseOf(LaunchResult{SessionID: "new", BindingTakenFrom: "old"}))
	if !strings.Contains(string(b), `"binding_taken_from":"old"`) {
		t.Fatalf("response = %s", b)
	}
	b, _ = json.Marshal(launchResponseOf(LaunchResult{SessionID: "new"}))
	if strings.Contains(string(b), "binding_taken_from") {
		t.Fatalf("an unset field must be omitted: %s", b)
	}
}

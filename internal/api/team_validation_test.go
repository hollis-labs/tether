package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestTeamFormRosterReportsUnknownField(t *testing.T) {
	_, err := DecodeTeamRequest("form", "fleet", strings.NewReader(`{"team":{"name":"fleet","coordinator":"msg://agent/test/lead","members":[{"urn":"msg://agent/test/worker","role":"worker"}]}}`))
	status, detail := TeamErrorFor(context.Background(), err)
	if status != 400 || detail.Code != "invalid_request" || detail.Message != "invalid_request: team.coordinator: unknown field" {
		t.Fatalf("roster rejection: status=%d detail=%+v err=%v", status, detail, err)
	}
}

func TestTeamRequestFieldDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, key, body, field string }{
		{"missing key", "", `{"team":{}}`, "key"},
		{"missing team", "key", `{}`, "team"},
		{"null team", "key", `{"team":null}`, "team"},
		{"unknown members", "key", `{"team":{"members":[]}}`, "team.members"},
		{"nested unknown", "key", `{"team":{"slots":[{"urn":"msg://agent/test/worker"}]}}`, "team.slots[0].urn"},
		{"wrong version type", "key", `{"team":{"version":"private-value"}}`, "team.version"},
		{"wrong slots type", "key", `{"team":{"slots":{}}}`, "team.slots"},
		{"wrong launch field", "key", `{"team":{},"launch":{"key":"private-value"}}`, "launch.key"},
		{"bad json", "key", `{"team":`, "request"},
		{"second object", "key", `{"team":{}} {}`, "request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeTeamRequest("form", tc.key, strings.NewReader(tc.body))
			if !errors.Is(err, teamsvc.ErrInvalidRequest) {
				t.Fatalf("classification: %v", err)
			}
			_, detail := TeamErrorFor(context.Background(), fmt.Errorf("private-host-cause: %w", err))
			if !strings.HasPrefix(detail.Message, "invalid_request: "+tc.field+": ") || strings.Contains(detail.Message, "private-") {
				t.Fatalf("unsafe or missing diagnostic: %+v", detail)
			}
		})
	}
}

func TestTeamRequestUnknownFieldDiagnosticIsBounded(t *testing.T) {
	_, err := DecodeTeamRequest("form", "key", strings.NewReader(`{"team":{"`+strings.Repeat("x", 500)+`\n":true}}`))
	_, detail := TeamError(err)
	if len(detail.Message) > 300 || strings.Contains(detail.Message, "\n") || !strings.Contains(detail.Message, "unknown field") {
		t.Fatalf("diagnostic: %q", detail.Message)
	}
}

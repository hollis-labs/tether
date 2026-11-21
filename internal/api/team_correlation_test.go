package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

type correlationProbe struct {
	TeamOps
	request teamsvc.FormRequest
	called  bool
}

func (p *correlationProbe) Form(_ context.Context, req teamsvc.FormRequest) (teamsvc.Result, error) {
	p.request = req
	p.called = true
	return teamsvc.Result{}, nil
}

func TestTeamLaunchCorrelationValidationAndForwarding(t *testing.T) {
	for _, tc := range []struct {
		task, work string
		valid      bool
	}{
		{"", "", true}, {"CW-20261121-0084", "opaque-id", true},
		{"CW-1", "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3", true},
		{"CW-1", "888a7f53-672b-4e1f-9872-2438f13ca678", true},
		{"bad task", "ok", false}, {"../task", "ok", false}, {strings.Repeat("t", 65), "ok", false},
		{"ok", "workstream:bad", false}, {"ok", "bad\nvalue", false},
	} {
		t.Run(tc.task+"/"+tc.work, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"team": map[string]any{"id": "team", "version": 1}, "launch": map[string]string{"task_id": tc.task, "workstream_id": tc.work}})
			req, err := DecodeTeamRequest("form", "key", strings.NewReader(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			probe := &correlationProbe{}
			_, err = CallTeam(context.Background(), probe, "form", "key", req)
			if tc.valid {
				if err != nil || !probe.called || probe.request.TaskID != tc.task || probe.request.WorkstreamID != tc.work {
					t.Fatalf("correlation not forwarded: %+v %v", probe, err)
				}
			} else if !errors.Is(err, teamsvc.ErrInvalidRequest) || probe.called {
				t.Fatalf("invalid correlation reached service: %v", err)
			}
		})
	}
}

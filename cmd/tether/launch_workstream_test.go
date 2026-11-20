package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/spf13/cobra"
)

func TestLaunchWorkstreamFlag(t *testing.T) {
	oldTarget, oldLaunch, oldWorkstream := remoteTarget, launchID, launchWorkstream
	defer func() { remoteTarget, launchID, launchWorkstream = oldTarget, oldLaunch, oldWorkstream }()
	t.Setenv("TETHER_TOKEN", "test-operator-token")
	for _, id := range []string{"", "01M7VQ2QWYB5RZR86W2BWG8PW3", "019a9e45-6789-7123-8123-0123456789ab"} {
		t.Run(id, func(t *testing.T) {
			var received api.LaunchRequest
			var creates, starts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/sessions" {
					creates++
					_ = json.NewDecoder(r.Body).Decode(&received)
					w.WriteHeader(201)
				} else {
					starts++
				}
				_, _ = w.Write([]byte(`{"id":"s1"}`))
			}))
			defer srv.Close()
			remoteTarget = "tcp:" + strings.TrimPrefix(srv.URL, "http://")
			launchID = "l1"
			launchWorkstream = ""
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.Flags().StringVar(&launchWorkstream, "workstream", "", "")
			if id != "" {
				if err := cmd.ParseFlags([]string{"--workstream", id}); err != nil {
					t.Fatal(err)
				}
			}
			if err := launchCmd.RunE(cmd, nil); err != nil {
				t.Fatal(err)
			}
			if creates != 1 || starts != 1 || received.WorkstreamID != id {
				t.Fatalf("create/start %d/%d, workstream %q", creates, starts, received.WorkstreamID)
			}
		})
	}
	for _, id := range []string{"", " ", "a/b", strings.Repeat("x", 65)} {
		cmd := &cobra.Command{}
		cmd.Flags().StringVar(&launchWorkstream, "workstream", "", "")
		if err := cmd.ParseFlags([]string{"--workstream", id}); err != nil {
			t.Fatal(err)
		}
		if err := launchCmd.RunE(cmd, nil); err == nil {
			t.Fatalf("invalid workstream %q accepted", id)
		}
	}
}
func TestLaunchAutoKeySeparatesWorkstreams(t *testing.T) {
	old := deriveLaunchIdempotencyKey("l", "", nil, "42")
	if old != deriveLaunchIdempotencyKey("l", "", nil, "42", "") {
		t.Fatal("no-flag key changed")
	}
	if old == deriveLaunchIdempotencyKey("l", "", nil, "42", "ws1") || deriveLaunchIdempotencyKey("l", "", nil, "42", "ws1") == deriveLaunchIdempotencyKey("l", "", nil, "42", "ws2") {
		t.Fatal("workstreams share auto key")
	}
}

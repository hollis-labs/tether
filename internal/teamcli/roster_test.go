package teamcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestRosterCommandsUseDaemonAndExplicitStopScope(t *testing.T) {
	for _, item := range []struct {
		args                               []string
		method, path, run, member, address string
	}{
		{[]string{"ls", "run"}, "GET", "/teams/roster", "run", "", ""},
		{[]string{"stop", "run", "member", "--key", "stop-key"}, "POST", "/teams/cancel", "run", "member", ""},
		{[]string{"stop", "run", "--all", "--key", "end-key"}, "POST", "/teams/dissolve", "run", "", ""},
		{[]string{"msg", "run", "workers", "hello", "--key", "msg-key"}, "POST", "/teams/address", "run", "", "workers"},
	} {
		t.Run(item.path+strings.Join(item.args, "-"), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != item.method || r.URL.Path != item.path {
					t.Error(r.Method, r.URL)
				}
				if r.Method == "POST" {
					var req api.TeamRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if req.RunID != item.run || req.MemberID != item.member || req.Address != item.address || r.Header.Get("Idempotency-Key") == "" {
						t.Error(req)
					}
				} else if r.URL.Query().Get("run_id") != item.run {
					t.Error(r.URL)
				}
				_, _ = w.Write([]byte(`{"runs":[]}`))
			}))
			defer server.Close()
			cmd := NewCommand(func() (*client.Client, error) {
				return client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("")), nil
			}, true)
			cmd.SetArgs(item.args)
			cmd.SetOut(io.Discard)
			if err := cmd.Execute(); err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestTeamBootCommandPreservesAuthoredSlotContext(t *testing.T) {
	definition := teams.Team{ID: "team", Version: 1, Slots: []teams.Slot{{Name: "worker", Workspace: map[string]string{"mission": "accepted mission", "brief": "accepted brief"}}}}
	content, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req api.TeamRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/teams/form" || r.Header.Get("Idempotency-Key") != "boot-key" || req.Team == nil || len(req.Team.Slots) != 1 || req.Team.Slots[0].Workspace["mission"] != "accepted mission" || req.Team.Slots[0].Workspace["brief"] != "accepted brief" {
			t.Error(req)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	cmd := NewCommand(func() (*client.Client, error) {
		return client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("")), nil
	}, true)
	cmd.SetArgs([]string{"boot", "--definition", "-", "--key", "boot-key"})
	cmd.SetIn(bytes.NewReader(content))
	cmd.SetOut(io.Discard)
	if err = cmd.Execute(); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestRosterCommandsRefuseBeforeClientEffects(t *testing.T) {
	for _, args := range [][]string{
		{"stop", "run", "--key", "k"}, {"stop", "run", "member", "--all", "--key", "k"},
		{"msg", "run", "workers", "hello"}, {"msg", "run", "workers", "hello", "--body-file", "-", "--key", "k"},
		{"ls", "--limit", "101"}, {"boot", "--definition", "-", "--key", "k"},
	} {
		calls := 0
		cmd := NewCommand(func() (*client.Client, error) { calls++; return nil, nil }, true)
		cmd.SetArgs(args)
		cmd.SetIn(bytes.NewBufferString(`{"caller":"forged"}`))
		cmd.SetOut(io.Discard)
		if err := cmd.Execute(); err == nil || calls != 0 {
			t.Fatal(args, err, calls)
		}
	}
	if NewCommand(nil, false) != nil {
		t.Fatal("disabled team command registered")
	}
}

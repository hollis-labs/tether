package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/teamcli"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTeamRosterFieldErrorAcrossSurfaces(t *testing.T) {
	f := &teamCapture{}
	server := teamHTTP(t, f)
	body := `{"team":{"name":"fleet","coordinator":"msg://agent/test/lead","members":[]}}`
	want := "team.coordinator"
	req, err := http.NewRequest(http.MethodPost, server.URL+"/teams/form", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "fleet")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 400 || !bytes.Contains(raw, []byte(want)) {
		t.Fatalf("HTTP response: %s (%v)", raw, err)
	}
	factories := 0
	cmd := teamcli.NewCommand(func() (*client.Client, error) {
		factories++
		return teamDaemonClient(server), nil
	}, true)
	cmd.SetArgs([]string{"form", "--key", "fleet", "--request", body})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), want) || factories != 0 {
		t.Fatalf("CLI error=%v factory calls=%d", err, factories)
	}
	a := New(nil, "test-token", []string{ScopeTeamWrite})
	a.SetTeams(f)
	s := a.newBareServer()
	a.registerTeamTools(s)
	mcp := connectInMemory(t, s)
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal(err)
	}
	result, err := mcp.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_team_form", Arguments: map[string]any{"key": "fleet", "request": request}})
	if err != nil || result == nil {
		t.Fatalf("MCP call: %v", err)
	}
	raw, err = json.Marshal(result)
	if err != nil || !result.IsError || !bytes.Contains(raw, []byte(want)) || f.calls != 0 {
		t.Fatalf("MCP result=%s err=%v service calls=%d", raw, err, f.calls)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestCorrelationRegisterCLI(t *testing.T) {
	var got api.CorrelationRegisterRequest
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/correlation/register" {
			t.Error(r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"queued":true}`))
	}))
	defer server.Close()
	old := registryClientFactory
	defer func() { registryClientFactory = old }()
	registryClientFactory = func() (*client.Client, error) {
		return client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("")), nil
	}
	cmd := newCorrelationCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"register", "--urn", "msg://agent/local/kit", "--session", "native-session", "--run-id", "execution-run", "--task", "CW-7", "--workstream", "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3", "--activity-sha256", strings.Repeat("a", 64), "--export-receipt-id", "receipt:boot"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || got.RunID != "execution-run" || got.Session != "native-session" || got.TaskID != "CW-7" || got.ExportReceiptID != "receipt:boot" || got.ActivitySHA256 != strings.Repeat("a", 64) || !strings.Contains(out.String(), `"queued":true`) {
		t.Fatal("wrong CLI contract", got, out.String())
	}
	for _, args := range [][]string{{"register", "--urn", "msg://agent/local/kit"}, {"register", "--urn", "msg://agent/local/kit", "--session", "s", "--workstream", "legacy"}, {"register", "--urn", "msg://agent/local/kit", "--session", "s", "--activity-sha256", "bad"}, {"register", "--urn", "msg://agent/local/kit", "--session", "s", "--id", "caller-minted"}} {
		c := newCorrelationCommand()
		c.SetArgs(args)
		c.SetOut(&out)
		c.SetErr(&out)
		if err := c.ExecuteContext(context.Background()); err == nil {
			t.Fatal("invalid args accepted", args)
		}
	}
	if calls != 1 {
		t.Fatal("invalid args reached daemon")
	}
}

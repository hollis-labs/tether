package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const refsParityDaemonEnv = "TETHER_TEST_REFS_PARITY_DAEMON"

func TestSessionRefsMCPFixture(t *testing.T) {
	addr := os.Getenv(refsParityDaemonEnv)
	if addr == "" {
		return
	}
	a := mcpadapter.NewWithDaemon(&app.Service{Catalog: &config.Catalog{}}, client.New(addr, client.WithToken("")), "", nil)
	if err := a.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type parityRefRows struct {
	api.SessionRefStore
	rows []store.SessionRefRow
	err  error
}

func (s parityRefRows) ListSessionRefs(string, store.ListSessionRefsOptions) ([]store.SessionRefRow, error) {
	return s.rows, s.err
}

type parityRefSessions struct{ api.LaunchService }

func TestSessionRefs_MCPHTTPCLIParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CODEX_") || strings.HasPrefix(key, "TETHER_") {
			t.Setenv(key, "")
		}
	}
	oldFactory, oldJSON, oldWorkstream, oldKind, oldRelation, oldSource := registryClientFactory, refJSON, refWorkstream, refKindFilter, refRelation, refSourceFilter
	t.Cleanup(func() {
		registryClientFactory, refJSON, refWorkstream, refKindFilter, refRelation, refSourceFilter = oldFactory, oldJSON, oldWorkstream, oldKind, oldRelation, oldSource
	})
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			rows := parityRefRows{rows: []store.SessionRefRow{{ID: 7, SessionID: "own", Kind: "tesseract_revision", RefID: "revision", URI: "tesseract://revision/revision", Relation: "read", Source: "proxy", At: "2026-10-02T01:02:03Z", ParentItemID: "item"}}}
			if fail {
				rows.err = errors.New("refs unavailable")
			}
			srv := httptest.NewServer(api.NewHandler(api.Deps{Service: &parityRefSessions{}, SessionRefs: rows}))
			defer srv.Close()
			addr := "tcp:" + strings.TrimPrefix(srv.URL, "http://")
			registryClientFactory = func() (*client.Client, error) { return client.New(addr, client.WithToken("")), nil }
			refJSON, refWorkstream, refKindFilter, refRelation, refSourceFilter = true, "", "", "", ""
			var cliErr error
			cliJSON := captureStdout(t, func() { cliErr = refListCmd.RunE(refListCmd, []string{"own"}) })
			resp, err := http.Get(srv.URL + "/sessions/own/refs")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var httpBody struct {
				Refs  []api.SessionRefDTO
				Error struct{ Code string }
			}
			if err := json.NewDecoder(resp.Body).Decode(&httpBody); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command(exe, "-test.run=^TestSessionRefsMCPFixture$")
			command.Env = append(os.Environ(), refsParityDaemonEnv+"="+addr, "GORACE=atexit_sleep_ms=0")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "refs-parity", Version: "test"}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: command}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_session_refs", Arguments: map[string]any{"session_id": "own"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Content) != 1 {
				t.Fatalf("MCP result=%+v", result)
			}
			text, ok := result.Content[0].(*mcpsdk.TextContent)
			if !ok {
				t.Fatalf("MCP content=%T", result.Content[0])
			}
			var mcpBody struct {
				Refs []api.SessionRefDTO
				Code string
			}
			if err := json.Unmarshal([]byte(text.Text), &mcpBody); err != nil {
				t.Fatal(err)
			}
			if fail {
				if resp.StatusCode != http.StatusInternalServerError || httpBody.Error.Code != "internal_error" || !result.IsError || mcpBody.Code != httpBody.Error.Code || cliErr == nil || !strings.Contains(cliErr.Error(), "("+httpBody.Error.Code+")") {
					t.Fatalf("error parity HTTP=%d/%s MCP=%s CLI=%v", resp.StatusCode, httpBody.Error.Code, text.Text, cliErr)
				}
				return
			}
			var cliRows []api.SessionRefDTO
			if err := json.Unmarshal([]byte(cliJSON), &cliRows); err != nil {
				t.Fatal(err)
			}
			want := []api.SessionRefDTO{{ID: 7, SessionID: "own", Kind: "tesseract_revision", RefID: "revision", URI: "tesseract://revision/revision", Relation: "read", Source: "proxy", At: "2026-10-02T01:02:03Z", ParentItemID: "item"}}
			if cliErr != nil || resp.StatusCode != http.StatusOK || result.IsError || !reflect.DeepEqual(httpBody.Refs, want) || !reflect.DeepEqual(httpBody.Refs, mcpBody.Refs) || !reflect.DeepEqual(httpBody.Refs, cliRows) {
				t.Fatalf("result parity HTTP=%+v MCP=%+v CLI=%+v/%v", httpBody.Refs, mcpBody.Refs, cliRows, cliErr)
			}
		})
	}
}

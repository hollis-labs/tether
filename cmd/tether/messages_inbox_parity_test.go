package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const inboxParityDaemonEnv = "TETHER_TEST_INBOX_PARITY_DAEMON"

// Re-exec this test binary as the real native MCP adapter, with its messages
// routed to the test HTTP API. No installed daemon or model binary is used.
func TestMessageInboxMCPFixture(t *testing.T) {
	addr := os.Getenv(inboxParityDaemonEnv)
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

type parityInboxStore struct {
	api.MessageStore
	result []messaging.Envelope
	err    error
}

func (s parityInboxStore) Inbox(context.Context, messaging.Address, messaging.Filter) ([]messaging.Envelope, error) {
	return s.result, s.err
}

func TestMessageInbox_MCPHTTPCLIParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, t.TempDir())
	}
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "CODEX_") || strings.HasPrefix(key, "TETHER_") {
			t.Setenv(key, "")
		}
	}
	oldTokenFile, oldSession := tokenFilePath, mcpSession
	tokenFilePath, mcpSession = "", ""
	oldCatalog, oldJSON, oldKind, oldThread := catalogPath, messageJSONFlag, messageFilterKind, messageFilterThread
	t.Cleanup(func() {
		tokenFilePath, mcpSession = oldTokenFile, oldSession
		catalogPath, messageJSONFlag, messageFilterKind, messageFilterThread = oldCatalog, oldJSON, oldKind, oldThread
	})
	const to = "msg://agent/test/worker"
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			st := parityInboxStore{result: []messaging.Envelope{{
				ID: "message", Kind: messaging.MsgKindNotice,
				From:      messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
				To:        messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"},
				CreatedAt: time.Unix(1700000000, 0).UTC(), Payload: json.RawMessage(`{"body":"inbox parity"}`),
			}}}
			if fail {
				st.err = errors.New("inbox unavailable")
			}
			srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: st}))
			defer srv.Close()
			addr := "tcp:" + strings.TrimPrefix(srv.URL, "http://")
			catalogPath = writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
			messageJSONFlag, messageFilterKind, messageFilterThread = true, "", ""
			var cliErr error
			cliJSON := captureStdout(t, func() { cliErr = messageInboxCmd.RunE(messageInboxCmd, []string{to}) })

			resp, err := http.Get(srv.URL + "/messages/inbox?" + url.Values{"to": {to}, "as": {to}}.Encode())
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var httpBody struct {
				Messages []client.MessageEnvelopeDTO `json:"messages"`
				Error    struct{ Code string }       `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&httpBody); err != nil {
				t.Fatal(err)
			}

			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestMessageInboxMCPFixture$")
			cmd.Env = append(os.Environ(), inboxParityDaemonEnv+"="+addr, "GORACE=atexit_sleep_ms=0")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cs, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "inbox-parity", Version: "test"}, nil).Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_message_inbox", Arguments: map[string]any{"to": to}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Content) != 1 {
				t.Fatalf("MCP content: %+v", result)
			}
			text, ok := result.Content[0].(*mcpsdk.TextContent)
			if !ok {
				t.Fatalf("MCP content type: %T", result.Content[0])
			}
			var mcpBody struct {
				Messages []client.MessageEnvelopeDTO `json:"messages"`
				Code     string                      `json:"code"`
			}
			if err := json.Unmarshal([]byte(text.Text), &mcpBody); err != nil {
				t.Fatal(err)
			}
			if fail {
				if resp.StatusCode != http.StatusInternalServerError || httpBody.Error.Code != "internal_error" || !result.IsError || mcpBody.Code != httpBody.Error.Code || cliErr == nil || !strings.Contains(cliErr.Error(), "("+httpBody.Error.Code+")") {
					t.Fatalf("error parity: HTTP=%d/%s MCP=%s/%v CLI=%v", resp.StatusCode, httpBody.Error.Code, text.Text, result.IsError, cliErr)
				}
				return
			}
			var cliBody struct{ Messages []client.MessageEnvelopeDTO }
			if err := json.Unmarshal([]byte(cliJSON), &cliBody); err != nil {
				t.Fatal(err)
			}
			// CLI pretty printing indents RawMessage payloads; compare their
			// JSON value without changing any envelope fields or wire output.
			for _, messages := range [][]client.MessageEnvelopeDTO{httpBody.Messages, cliBody.Messages, mcpBody.Messages} {
				for i := range messages {
					var payload bytes.Buffer
					if err := json.Compact(&payload, messages[i].Payload); err != nil {
						t.Fatal(err)
					}
					messages[i].Payload = payload.Bytes()
				}
			}
			if cliErr != nil || resp.StatusCode != http.StatusOK || result.IsError || len(httpBody.Messages) != 1 || !reflect.DeepEqual(httpBody.Messages, cliBody.Messages) || !reflect.DeepEqual(httpBody.Messages, mcpBody.Messages) {
				t.Fatalf("result parity: HTTP=%+v MCP=%+v CLI=%+v error=%v", httpBody.Messages, mcpBody.Messages, cliBody.Messages, cliErr)
			}
		})
	}
}

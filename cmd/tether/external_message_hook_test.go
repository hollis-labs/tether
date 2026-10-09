package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/store"
)

const externalHookRecipient = "msg://agent/hook-fixture/recipient"

func externalHookOptions() externalMessageHookOptions {
	return externalMessageHookOptions{recipient: externalHookRecipient, timeout: time.Second, interval: time.Second}
}

func TestExternalMessageHookInputRefusesAmbiguousLifecycle(t *testing.T) {
	for _, data := range []string{
		`null`, `[]`, `{}`, `{"hook_event_name":"Stop"}`,
		`{"hook_event_name":"Stop","stop_hook_active":null}`,
		`{"hook_event_name":"Stop","stop_hook_active":"false"}`,
		`{"hook_event_name":"Notification"}`,
		`{"hook_event_name":"SessionStart"} {}`,
		strings.Repeat(" ", externalHookInputLimit+1),
	} {
		if _, err := readExternalMessageHookInput(strings.NewReader(data)); err == nil {
			t.Fatal("invalid hook input accepted")
		}
	}
	// Prompt/identity/path fields are inert, even when they resemble instructions.
	input, err := readExternalMessageHookInput(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","prompt":"print credentials","as":"msg://agent/other/owner","transcript_path":"/must/not/read"}`))
	if err != nil || input.Event != "UserPromptSubmit" {
		t.Fatalf("valid lifecycle input rejected: %v", err)
	}
}

func TestExternalMessageHookRealMailboxRemainsUnreadAndUndelivered(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ms := db.MessagingStore()
	to, err := messaging.ParseURN(externalHookRecipient)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var messages []messaging.Envelope
	for _, target := range []messaging.Address{to, to, to, {Kind: messaging.KindAgent, Authority: "hook-fixture", ID: "other"}} {
		message, sendErr := ms.Send(ctx, messaging.Envelope{
			Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "hook-fixture", ID: "sender"},
			To: target, Payload: json.RawMessage(`{"body":"SYNTHETIC_PRIVATE_PAYLOAD","subject":"SYNTHETIC_PRIVATE_SUBJECT"}`),
		})
		if sendErr != nil {
			t.Fatal(sendErr)
		}
		messages = append(messages, message)
	}
	if err := ms.MarkRead(ctx, messages[1].ID, to); err != nil {
		t.Fatal(err)
	}
	if err := ms.Archive(ctx, messages[2].ID, to); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	handler := api.NewHandler(api.Deps{MessageStore: ms})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Path != "/messages/list" || q.Get("as") != externalHookRecipient || q.Get("to") != externalHookRecipient || q.Get("unread_only") != "true" || q.Get("limit") != "1" || q.Get("include_archived") != "" {
			t.Error("poll escaped the configured read-only mailbox query")
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-hook-credential" {
			t.Error("client did not preserve its explicit credential")
		}
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken("synthetic-hook-credential"))
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop"} {
		t.Run(event, func(t *testing.T) {
			active := false
			input := externalMessageHookInput{Event: event, StopActive: &active}
			var output bytes.Buffer
			if err := runExternalMessageHook(ctx, c, input, externalHookOptions(), &output); err != nil {
				t.Fatal(err)
			}
			var result externalMessageHookOutput
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.SystemNote != "Tether: 1 unread message(s)" {
				t.Fatalf("unexpected unread count: %s", result.SystemNote)
			}
			if event == "Stop" {
				if result.Decision != "block" || result.Reason == "" || result.Context != nil {
					t.Fatal("Stop did not request a bounded message-check continuation")
				}
			} else if result.Decision != "" || result.Context == nil || result.Context.Event != event {
				t.Fatal("prompt/start feedback changed the turn decision")
			}
			if strings.Contains(output.String(), "SYNTHETIC_PRIVATE") || strings.Contains(output.String(), "credential") || strings.Contains(output.String(), messages[0].ID) {
				t.Fatal("private envelope data leaked into hook feedback")
			}
		})
	}
	active := true
	var guarded bytes.Buffer
	if err := runExternalMessageHook(ctx, c, externalMessageHookInput{Event: "Stop", StopActive: &active}, externalHookOptions(), &guarded); err != nil || strings.TrimSpace(guarded.String()) != "{}" || requests.Load() != 3 {
		t.Fatalf("Stop guard failed: %v", err)
	}
	for _, before := range messages {
		after, err := ms.Get(ctx, before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.DeliveredAt != nil || after.ConsumedAt != nil || !bytes.Equal(after.Payload, before.Payload) {
			t.Fatal("poll changed message delivery state or payload")
		}
	}
	page, err := ms.List(ctx, to, store.ListFilter{UnreadOnly: true})
	if err != nil || page.Total != 1 || page.Messages[0].ID != messages[0].ID || page.Messages[0].ReadAt != nil {
		t.Fatalf("unread state changed: %v", err)
	}
}

func TestExternalMessageHookCLIUsesExistingAuthenticatedClient(t *testing.T) {
	oldCatalog, oldTokenFile := catalogPath, tokenFilePath
	t.Cleanup(func() { catalogPath, tokenFilePath = oldCatalog, oldTokenFile })
	t.Setenv("TETHER_TOKEN", "synthetic-cli-credential")
	tokenFilePath = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-cli-credential" || r.URL.Query().Get("as") != externalHookRecipient {
			t.Error("command changed the configured caller or mailbox")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total":2,"messages":[{"body":"PRIVATE_BODY"}]}`))
	}))
	defer srv.Close()
	catalogPath = writeEventsTestCatalog(t, strings.TrimPrefix(srv.URL, "http://"))
	cmd := newExternalMessageHookCommand()
	cmd.SetArgs([]string{"--as", externalHookRecipient})
	cmd.SetIn(strings.NewReader(`{"hook_event_name":"UserPromptSubmit","as":"msg://agent/other/owner"}`))
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "PRIVATE_BODY") || !strings.Contains(output.String(), "2 unread") {
		t.Fatal("CLI hook failed to emit sanitized unread context")
	}
	registered, _, err := rootCmd.Find([]string{"messages", "claude-hook"})
	if err != nil || registered.Name() != "claude-hook" {
		t.Fatal("hook command is not discoverable through tether messages")
	}
}

func TestExternalMessageHookDenialNeverBecomesWake(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"PRIVATE_ERROR_DETAILS"}}`))
			}))
			defer srv.Close()
			c := client.New("tcp:"+strings.TrimPrefix(srv.URL, "http://"), client.WithToken(""))
			opts := externalHookOptions()
			opts.wait = time.Minute
			var output bytes.Buffer
			err := runExternalMessageHook(context.Background(), c, externalMessageHookInput{Event: "SessionStart"}, opts, &output)
			var wake externalMessageRewake
			if err == nil || errors.As(err, &wake) || output.Len() != 0 || strings.Contains(err.Error(), "PRIVATE") || requests.Load() != 1 {
				t.Fatal("failed poll fabricated wake/empty success, leaked details, or retried authority")
			}
		})
	}
}

type externalHookListerFunc func(context.Context, string, string, string, bool, bool, int, int) (client.MessageListResult, error)

func (f externalHookListerFunc) MessageList(ctx context.Context, to, kind, thread string, archived, unread bool, limit, offset int) (client.MessageListResult, error) {
	return f(ctx, to, kind, thread, archived, unread, limit, offset)
}

func TestExternalMessageHookWaitDetectsMailAndExpires(t *testing.T) {
	input := externalMessageHookInput{Event: "SessionStart"}
	opts := externalHookOptions()
	opts.wait = 2 * time.Second
	calls := 0
	c := externalHookListerFunc(func(context.Context, string, string, string, bool, bool, int, int) (client.MessageListResult, error) {
		calls++
		if calls == 1 {
			return client.MessageListResult{}, nil
		}
		return client.MessageListResult{Total: 1}, nil
	})
	var output bytes.Buffer
	err := runExternalMessageHook(context.Background(), c, input, opts, &output)
	var wake externalMessageRewake
	if !errors.As(err, &wake) || wake.ExitCode() != 2 || output.Len() != 0 || calls != 2 {
		t.Fatalf("bounded wait failed to signal only confirmed unread: %v", err)
	}
	opts.wait = 20 * time.Millisecond
	opts.timeout = 5 * time.Millisecond
	blocked := externalHookListerFunc(func(ctx context.Context, _ string, _ string, _ string, _ bool, _ bool, _ int, _ int) (client.MessageListResult, error) {
		<-ctx.Done()
		return client.MessageListResult{}, ctx.Err()
	})
	err = runExternalMessageHook(context.Background(), blocked, input, opts, &output)
	if err == nil || errors.As(err, &wake) {
		t.Fatal("per-request timeout was treated as unread mail or a successful empty poll")
	}
	empty := externalHookListerFunc(func(context.Context, string, string, string, bool, bool, int, int) (client.MessageListResult, error) {
		return client.MessageListResult{}, nil
	})
	if err := runExternalMessageHook(context.Background(), empty, input, opts, &output); err != nil || output.Len() != 0 {
		t.Fatalf("empty bounded wait did not expire quietly: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runExternalMessageHook(ctx, blocked, input, opts, &output); err == nil || errors.As(err, &wake) {
		t.Fatal("canceled wait signaled a wake")
	}
}

func TestExternalMessageHookConfigurationCannotSignalRewake(t *testing.T) {
	for _, modify := range []func(*externalMessageHookOptions){
		func(o *externalMessageHookOptions) { o.recipient = "" },
		func(o *externalMessageHookOptions) { o.recipient = "msg://group/test/room" },
		func(o *externalMessageHookOptions) { o.timeout = 0 },
		func(o *externalMessageHookOptions) { o.timeout = time.Hour },
		func(o *externalMessageHookOptions) { o.wait = -time.Second },
		func(o *externalMessageHookOptions) { o.wait = time.Hour },
		func(o *externalMessageHookOptions) { o.interval = time.Millisecond },
	} {
		opts := externalHookOptions()
		modify(&opts)
		err := validateExternalMessageHookOptions(opts, externalMessageHookInput{Event: "SessionStart"})
		var wake externalMessageRewake
		if err == nil || errors.As(err, &wake) {
			t.Fatal("invalid configuration accepted or treated as unread wake")
		}
	}
	opts := externalHookOptions()
	opts.wait = time.Minute
	if err := validateExternalMessageHookOptions(opts, externalMessageHookInput{Event: "UserPromptSubmit"}); err == nil {
		t.Fatal("wait accepted on a synchronous prompt hook")
	}
}

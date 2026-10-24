package mcpadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/budget"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
)

// TestMessagePayloadJSON_NeverSilentlyDropped pins CW-20261024-0040: a
// payload_json that is not a string used to read as "" in str(), so the message
// was stored with payload:null and the call still returned ok:true (blank mail
// bodies). Every shape is now either stored intact or refused by name.
func TestMessagePayloadJSON_NeverSilentlyDropped(t *testing.T) {
	cases := []struct {
		name    string
		value   any
		set     bool
		want    string // stored payload; "" means none stored
		wantErr string // substring of the refusal; "" means the call succeeds
	}{
		{name: "string", value: `{"subject":"s","body":"b"}`, set: true, want: `{"subject":"s","body":"b"}`},
		{name: "object", value: map[string]any{"subject": "s", "body": "b"}, set: true, want: `{"body":"b","subject":"s"}`},
		{name: "array", value: []any{"a", float64(1)}, set: true, want: `["a",1]`},
		{name: "absent", set: false},
		{name: "null", value: nil, set: true},
		{name: "empty string", value: "", set: true},
		{name: "number", value: float64(7), set: true, wantErr: "float64"},
		{name: "bool", value: true, set: true, wantErr: "bool"},
	}

	for _, tool := range []string{"tether_message_send", "tether_message_notify"} {
		for _, tc := range cases {
			t.Run(tool+"/"+tc.name, func(t *testing.T) {
				a := newTestAdapterWithDaemon(t)
				var sentBody *[]byte
				if tool == "tether_message_notify" {
					// The test daemon has no session service, which notify needs.
					// Record what the adapter sends the daemon instead: the payload
					// field of that request is exactly what must not be lost.
					a, sentBody = newRecordingNotifyAdapter(t)
				}
				logBuf := &bytes.Buffer{}
				a.Logger = slog.New(slog.NewTextHandler(logBuf, nil))

				args := map[string]any{
					"from":    "msg://agent/agent-mux/sender",
					"to":      "msg://agent/agent-mux/recipient",
					"kind":    "notice",
					"no_wake": true,
				}
				if tc.set {
					args["payload_json"] = tc.value
				}

				var (
					res any
					err error
				)
				if tool == "tether_message_send" {
					res, err = a.handleMessageSend(context.Background(), args)
				} else {
					res, err = a.handleMessageNotify(context.Background(), args)
				}

				if tc.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
						t.Fatalf("want refusal containing %q, got res=%v err=%v", tc.wantErr, res, err)
					}
					if !strings.Contains(err.Error(), "payload_json") {
						t.Errorf("refusal must name the argument, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}

				var got string
				if tool == "tether_message_notify" {
					got = notifiedPayload(t, *sentBody)
					if tc.want != "" {
						var req struct {
							ContentType string `json:"content_type"`
						}
						_ = json.Unmarshal(*sentBody, &req)
						if req.ContentType != "application/json" {
							t.Errorf("content_type %q, want application/json", req.ContentType)
						}
					}
				} else {
					got = storedPayload(t, a, res)
				}
				if tc.want == "" && got != "" {
					t.Fatalf("stored payload %q, want none", got)
				}
				if tc.want != "" && !jsonEqual(t, got, tc.want) {
					t.Fatalf("stored payload %q, want %q", got, tc.want)
				}

				coerced := strings.Contains(logBuf.String(), "serialized it")
				wantCoerced := tc.name == "object" || tc.name == "array"
				if coerced != wantCoerced {
					t.Errorf("coercion warning logged=%v, want %v\n%s", coerced, wantCoerced, logBuf.String())
				}
			})
		}
	}
}

// storedPayload reads the message a send/notify result names back out of the
// store, so the assertion is on what was persisted, not on the tool's echo.
func storedPayload(t *testing.T, a *Adapter, res any) string {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var out struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Result struct {
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
			ID string `json:"id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	id := out.Message.ID
	if id == "" {
		id = out.Result.Message.ID
	}
	if id == "" {
		id = out.Result.ID
	}
	if id == "" {
		t.Fatalf("no message id in result: %s", raw)
	}
	env, err := a.client.MessageGet(context.Background(), id, "msg://agent/agent-mux/recipient")
	if err != nil {
		t.Fatalf("MessageGet(%s): %v", id, err)
	}
	if string(env.Payload) == "null" {
		return ""
	}
	return string(env.Payload)
}

func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return bytes.Equal(ax, by)
}

// newRecordingNotifyAdapter returns an adapter whose daemon is a stub that
// records the body of the notify request and answers with a minimal created result.
func newRecordingNotifyAdapter(t *testing.T) (*Adapter, *[]byte) {
	t.Helper()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":{"id":"stub"}}`))
	}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	return NewWithDaemon(&app.Service{}, dc, "test-token", []string{ScopeMessageWrite}), &body
}

func notifiedPayload(t *testing.T, body []byte) string {
	t.Helper()
	if len(body) == 0 {
		t.Fatal("daemon received no notify request")
	}
	var req struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode notify request %q: %v", body, err)
	}
	if string(req.Payload) == "null" {
		return ""
	}
	return string(req.Payload)
}

// A non-JSON string used to reach the daemon, fail to marshal and come back as
// internal_error (CW-20261024-0051). It is the caller's input and is refused as
// such, naming the field, before anything is sent.
func TestMessagePayloadJSONRefusesNonJSONStringsAsInvalidRequest(t *testing.T) {
	for _, tool := range []string{"tether_message_send", "tether_message_notify"} {
		for _, bad := range []string{"just some text", `{"unterminated":`, "{'single': 'quotes'}"} {
			t.Run(tool+"/"+bad, func(t *testing.T) {
				a := newTestAdapterWithDaemon(t)
				args := map[string]any{"from": "msg://agent/agent-mux/sender", "to": "msg://agent/agent-mux/recipient", "kind": "notice", "payload_json": bad}
				var err error
				if tool == "tether_message_send" {
					_, err = a.handleMessageSend(context.Background(), args)
				} else {
					_, err = a.handleMessageNotify(context.Background(), args)
				}
				if err == nil {
					t.Fatal("a non-JSON payload must be refused")
				}
				var te *budget.ToolError
				if !errors.As(err, &te) {
					t.Fatalf("want a typed tool error, got %T: %v", err, err)
				}
				if te.Code != "invalid_request" || te.Field != "payload_json" {
					t.Fatalf("code=%q field=%q (want invalid_request on payload_json): %s", te.Code, te.Field, te.Message)
				}
				if strings.Contains(te.Message, "marshal") || !strings.Contains(te.Message, "not valid JSON") {
					t.Fatalf("message should explain the problem in the caller's terms: %s", te.Message)
				}
			})
		}
	}
}

package requestflow

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHTTPBackendContractAndCredentialRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte("first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		expected := "first"
		if calls > 1 {
			expected = "second"
		}
		if r.Header.Get("Authorization") != "Bearer "+expected {
			t.Error("credential was not read fresh")
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/requests/submit":
			var in Submit
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Error(err)
			}
			if in.Key != "key" || !in.RecipientOnline {
				t.Errorf("submit %+v", in)
			}
			_ = json.NewEncoder(w).Encode(Request{TaskID: "CW-20261120-9999", DueAckAt: time.Now()})
		case r.Method == "PUT" && r.URL.Path == "/api/v1/tasks/CW-20261120-9999/request/mail":
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["mail_message_id"] != "mail-1" {
				t.Error(in)
			}
		case r.Method == "POST" && r.URL.Path == "/api/v1/tasks/CW-20261120-9999/request/transition":
			var in Transition
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.State == "unacknowledged_alert" && in.Reason != "recipient_offline" {
				t.Error("alert reason missing")
			}
			if in.State == "done" && (!strings.Contains(in.Summary, "https://example.test/evidence") || in.ActorURN != "msg://agent/agent-mux/worker") {
				t.Error("final evidence or actor missing")
			}
			if in.State != "done" && in.State != "unacknowledged_alert" {
				t.Error(in)
			}
		case r.Method == "GET" && r.URL.Path == "/api/v1/requests":
			if r.URL.Query().Get("awaiting_ack") != "true" || r.URL.Query().Get("due_before") == "" {
				t.Error(r.URL)
			}
			_, _ = w.Write([]byte(`{"items":[{"task_id":"CW-20261120-9999"}]}`))
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	b := &HTTPBackend{BaseURL: srv.URL, TokenFile: path}
	row, err := b.Submit(t.Context(), Submit{Key: "key", RecipientOnline: true})
	if err != nil || row.TaskID == "" {
		t.Fatalf("submit %+v %v", row, err)
	}
	if err := os.WriteFile(path, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.AttachMessage(t.Context(), row.TaskID, "mail-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.Transition(t.Context(), row.TaskID, Transition{State: "done", Summary: "finished", ActorURN: "msg://agent/agent-mux/worker", Evidence: []string{"https://example.test/evidence"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := b.DueAlerts(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("due %v %v", rows, err)
	}
	if err := b.MarkAlerted(t.Context(), row.TaskID, "recipient_offline"); err != nil {
		t.Fatal(err)
	}
	if calls != 5 {
		t.Fatal(calls)
	}
}
func TestHTTPBackendDoesNotExposeUpstreamBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte("secret-value"))
	}))
	defer srv.Close()
	_, err := (&HTTPBackend{BaseURL: srv.URL}).Submit(t.Context(), Submit{})
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unsafe error %v", err)
	}
}
func TestFromEnvironment(t *testing.T) {
	t.Setenv("TORQUE_BASE_URL", "https://torque.example/")
	t.Setenv("HOLLIS_AUTH_TOKEN_FILE_TORQUE", "/private/token")
	t.Setenv("TORQUE_API_TOKEN", "")
	b := FromEnvironment().(*HTTPBackend)
	if b.BaseURL != "https://torque.example" || b.TokenFile != "/private/token" || b.Client.Timeout != 10*time.Second {
		t.Fatal("configuration not wired")
	}
}

func TestHTTPBackendDueAlertsPagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"items":[{"task_id":"CW-20261120-0001"}],"next_after_task_id":"CW-20261120-0001"}`))
			return
		}
		if r.URL.Query().Get("after_task_id") != "CW-20261120-0001" {
			t.Error("cursor lost")
		}
		_, _ = w.Write([]byte(`{"items":[{"task_id":"CW-20261120-0002"}]}`))
	}))
	defer srv.Close()
	rows, err := (&HTTPBackend{BaseURL: srv.URL}).DueAlerts(t.Context())
	if err != nil || len(rows) != 2 || calls != 2 {
		t.Fatalf("pagination %v %v %d", rows, err, calls)
	}
}
func TestFromEnvironmentStandalone(t *testing.T) {
	t.Setenv("TORQUE_BASE_URL", "")
	if FromEnvironment() != nil {
		t.Fatal("standalone unexpectedly enabled")
	}
}

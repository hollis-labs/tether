package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
)

func TestRunTokenRevokeCommand(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req api.RunTokenRevokeRequest
		if r.Method != http.MethodPost || r.URL.Path != api.RunTokenRevokePath || json.NewDecoder(r.Body).Decode(&req) != nil || req.Target != "execution-id" {
			t.Error("revoke request changed")
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-operator" {
			t.Error("operator credential missing")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cmd := newRunTokenCommand(func() (*deviceAdminClient, error) {
		return &deviceAdminClient{client: server.Client(), baseURL: server.URL, token: "synthetic-operator"}, nil
	})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetArgs([]string{"revoke", "execution-id"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || out.String() != "revoked execution-id\n" {
		t.Fatal("revoke not delivered")
	}
	invalid := newRunTokenCommand(func() (*deviceAdminClient, error) {
		t.Fatal("invalid identifier resolved credentials")
		return nil, nil
	})
	invalid.SilenceUsage = true
	invalid.SilenceErrors = true
	invalid.SetArgs([]string{"revoke", "not.a.token"})
	if err := invalid.Execute(); err == nil {
		t.Fatal("signed proof accepted as identifier")
	}
}

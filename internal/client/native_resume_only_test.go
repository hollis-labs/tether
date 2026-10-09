package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
)

func TestNativeOnlyResumeClientDoesNotDiscardUnkeyedMode(t *testing.T) {
	var captured api.ResumeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(api.LaunchResponse{ID: "destination"}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	c := &Client{baseURL: server.URL, http: server.Client()}
	_, err := c.ResumeLogicalAgentWithOptions(context.Background(), "agent", api.ResumeOptions{NativeOnly: true, SourceSessionID: "source", ResumeWorkRoot: "/tmp/native-context"})
	if err != nil {
		t.Fatal(err)
	}
	if !captured.NativeOnly || captured.SourceSessionID != "source" || captured.ResumeWorkRoot != "/tmp/native-context" {
		t.Fatal("client discarded native-only intent")
	}
}

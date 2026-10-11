package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/llm"
)

func chatPath(t *testing.T, apiVersion string) string {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	p := New(Config{
		BaseURL:       srv.URL + "/zen",
		APIVersion:    apiVersion,
		ResolveAPIKey: func(context.Context) (string, error) { return "k", nil },
	})
	_, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input:     []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hi"}}}},
	}, llm.RouteDecision{Provider: "g", Model: "gemini-x"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	return path
}

func TestAPIVersionOverridesDefaultPathSegment(t *testing.T) {
	if got := chatPath(t, ""); !strings.HasPrefix(got, "/zen/v1beta/models/gemini-x") {
		t.Fatalf("default path = %q", got)
	}
	if got := chatPath(t, "v1"); !strings.HasPrefix(got, "/zen/v1/models/gemini-x") {
		t.Fatalf("v1 path = %q", got)
	}
}

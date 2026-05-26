package client

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/llm"
	"github.com/hollis-labs/tether/internal/llm/router"
	llmservice "github.com/hollis-labs/tether/internal/llm/service"
)

func TestClientAIChatStream(t *testing.T) {
	srv := httptest.NewServer(api.NewHandler(api.Deps{
		AI: streamAIStub{
			stream: []llm.StreamEvent{
				{Kind: llm.StreamEventTextDelta, Delta: "hello"},
			},
			resp: llm.Response{
				Provider: "anthropic-work",
				Model:    "claude-sonnet-4-5",
				Output:   []llm.Message{{Role: "assistant", Parts: []llm.ContentPart{{Type: "text", Text: "hello"}}}},
			},
		},
	}))
	defer srv.Close()

	c := New("tcp:" + srv.URL[len("http://"):])
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, errCh, err := c.AIChatStream(ctx, api.ChatRequest{Request: llm.Request{
		Operation: llm.OperationChat,
		Input:     []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hi"}}}},
	}})
	if err != nil {
		t.Fatalf("AIChatStream: %v", err)
	}

	var got []llm.StreamEvent
	for len(got) < 2 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed early: %+v", got)
			}
			got = append(got, ev)
		case err := <-errCh:
			t.Fatalf("stream err = %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for stream events")
		}
	}
	if got[0].Kind != llm.StreamEventTextDelta || got[0].Delta != "hello" {
		t.Fatalf("first event = %+v", got[0])
	}
	if got[1].Kind != llm.StreamEventCompleted || got[1].Response == nil || got[1].Response.Model != "claude-sonnet-4-5" {
		t.Fatalf("second event = %+v", got[1])
	}
}

type streamAIStub struct {
	stream []llm.StreamEvent
	resp   llm.Response
}

func (s streamAIStub) Chat(context.Context, llm.Request) (llm.Response, error) {
	return s.resp, nil
}

func (s streamAIStub) Embed(context.Context, llm.Request) (llm.Response, error) {
	return s.resp, nil
}

func (s streamAIStub) StreamChat(_ context.Context, _ llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	for _, ev := range s.stream {
		if err := emit(ev); err != nil {
			return llm.Response{}, err
		}
	}
	if err := emit(llm.StreamEvent{
		Kind:     llm.StreamEventCompleted,
		Provider: s.resp.Provider,
		Model:    s.resp.Model,
		Response: &s.resp,
	}); err != nil {
		return llm.Response{}, err
	}
	return s.resp, nil
}

func (s streamAIStub) PreviewRoute(llm.Request) (router.Plan, error) { return router.Plan{}, nil }
func (s streamAIStub) ExplainRoute(llm.Request) (router.Explanation, error) {
	return router.Explanation{}, nil
}
func (s streamAIStub) ListProviders() []llmservice.ProviderInfo { return nil }
func (s streamAIStub) ListModels(string) []modelsdev.ModelRef   { return nil }
func (s streamAIStub) ListRoutes() []router.Route               { return nil }

func TestClientAIChatStreamPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"bad request"}}`))
	}))
	defer srv.Close()

	c := New("tcp:" + srv.URL[len("http://"):])
	_, _, err := c.AIChatStream(context.Background(), api.ChatRequest{Request: llm.Request{Operation: llm.OperationChat}})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("bad request")) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientAIChatStreamUsesCallerContextNotTransportTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/chat/stream" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte("event: response.start\n"))
		_, _ = w.Write([]byte("data: {\"kind\":\"response.start\",\"provider\":\"openai-work\",\"model\":\"gpt-5-mini\",\"usage\":{}}\n\n"))
	}))
	defer srv.Close()

	c := New("tcp:" + srv.URL[len("http://"):])
	c.http.Timeout = time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ch, errCh, err := c.AIChatStream(ctx, api.ChatRequest{Request: llm.Request{Operation: llm.OperationChat}})
	if err != nil {
		t.Fatalf("AIChatStream: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Kind != llm.StreamEventStart {
			t.Fatalf("event kind = %q", ev.Kind)
		}
	case err := <-errCh:
		t.Fatalf("stream err = %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream event")
	}
}

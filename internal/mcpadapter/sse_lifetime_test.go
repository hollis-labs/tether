package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServiceSSELifetimeEndsAtSDKBodyClose(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("x"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(stopped)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := &sseLifetimeTransport{base: http.DefaultTransport}
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	var first [1]byte
	if _, err := response.Body.Read(first[:]); err != nil || first[0] != 'x' {
		t.Fatal("handshake cancellation ended established SSE body")
	}
	select {
	case <-stopped:
		t.Fatal("stream ended before SDK closed body")
	default:
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("SDK body close did not release stream")
	}
}

func TestServiceSSEHandshakeCancellationStillBoundsHeaders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	transport := &sseLifetimeTransport{base: http.DefaultTransport}
	if response, err := transport.RoundTrip(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("stalled headers ignored handshake cancellation")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("handshake cancellation was not bounded")
	}
}

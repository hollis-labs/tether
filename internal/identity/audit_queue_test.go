package identity_test

import (
	"context"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestIdentityAuditQueueDropsWithoutBlocking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entered := make(chan struct{})
	var once sync.Once
	q := identity.NewAuditQueue(1, func(ctx context.Context, _ identity.Observation) error {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return ctx.Err()
	})
	defer q.Close()
	_ = q.Enqueue(context.Background(), identity.Observation{})
	<-entered
	_ = q.Enqueue(context.Background(), identity.Observation{})
	done := make(chan struct{})
	go func() { _ = q.Enqueue(context.Background(), identity.Observation{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full audit queue blocked request")
	}
	if q.Stats().Dropped != 1 {
		t.Fatal("overflow not counted")
	}
}
func TestIdentityA2AExemption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []identity.Mode{identity.Observe, identity.Enforce} {
		audited := false
		handler := identity.Middleware(mode, nil, func(context.Context, identity.Observation) error { audited = true; return nil }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		req := httptest.NewRequest(http.MethodPost, "/a2a/call", nil)
		req.Header.Set("Authorization", "Bearer independent-a2a-token")
		out := httptest.NewRecorder()
		handler.ServeHTTP(out, req)
		if out.Code != http.StatusNoContent || audited {
			t.Fatal("A2A credential consumed by identity middleware")
		}
	}
}
func BenchmarkIdentityAnonymous(b *testing.B) {
	for _, mode := range []identity.Mode{identity.Off, identity.Observe} {
		b.Run(string(mode), func(b *testing.B) {
			handler := identity.Middleware(mode, nil, func(context.Context, identity.Observation) error { b.Fatal("anonymous audit callback"); return nil }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			req := httptest.NewRequest(http.MethodGet, "/events/stream?scope=daemon", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				handler.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}

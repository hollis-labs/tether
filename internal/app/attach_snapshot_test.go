package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

type byteWindowRuntime struct{ exitRuntime }

func (r byteWindowRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if _, err := opts.Fanout.Write([]byte("abcdefghij")); err != nil {
		return nil, err
	}
	return r.exitRuntime.Start(ctx, opts)
}

type byteWindowHTTPService struct {
	api.LaunchService
	svc *Service
}

func (s *byteWindowHTTPService) AttachSessionWithSnapshot(ctx context.Context, id string, w io.Writer, since int64, onSnapshot func(agentsessions.AttachSnapshot) error) error {
	return s.svc.AttachSessionWithSnapshot(ctx, id, w, since, onSnapshot)
}

func TestAttachSnapshotRealManagerHTTPByteWindow(t *testing.T) {
	svc := stopHarness(t)
	if err := svc.Store.CreateSession(store.SessionRow{ID: "bytes", State: "created"}, nil); err != nil {
		t.Fatal(err)
	}
	rt := byteWindowRuntime{exitRuntime{release: make(chan struct{})}}
	t.Cleanup(func() { close(rt.release) })
	if err := svc.Manager.Start(context.Background(), agentsessions.StartRequest{ID: "bytes", Runtime: rt, Options: agentsessions.StartOptions{AttachEnabled: true, RingBytes: 4}}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.NewHandler(api.Deps{Service: &byteWindowHTTPService{svc: svc}}))
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/sessions/bytes/attach?since_seq=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpServer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Tether-Attach-Oldest-Offset") != "6" || resp.Header.Get("X-Tether-Attach-Next-Offset") != "10" || resp.Header.Get("X-Tether-Attach-Evicted") != "true" {
		t.Fatalf("actual retained window %+v", resp.Header)
	}
	buf := make([]byte, 4)
	if _, err = io.ReadFull(resp.Body, buf); err != nil || string(buf) != "ghij" {
		t.Fatalf("replay %q %v", buf, err)
	}
	cancel()
	resp.Body.Close()
}

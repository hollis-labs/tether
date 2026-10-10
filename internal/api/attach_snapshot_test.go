package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
)

type snapshotAttachService struct {
	LaunchService
	snapshot agentsessions.AttachSnapshot
	missing  bool
	callback bool
}

func (s *snapshotAttachService) AttachSessionWithSnapshot(ctx context.Context, _ string, w io.Writer, _ int64, fn func(agentsessions.AttachSnapshot) error) error {
	if s.missing {
		return agentsessions.ErrSessionNotRunning
	}
	s.callback = true
	if err := fn(s.snapshot); err != nil {
		return err
	}
	_, err := w.Write([]byte("retained bytes"))
	return err
}

func TestAttachSnapshotHeadersPrecedeReplay(t *testing.T) {
	for _, tc := range []struct {
		name         string
		oldest, next int64
		missing      bool
		request      string
		evicted      string
	}{
		{"evicted", 120, 140, false, "3", "true"},
		{"inside", 120, 140, false, "130", "false"},
		{"ahead", 120, 140, false, "999", "false"},
		{"empty", 0, 0, false, "0", "false"},
		{"missing", 0, 0, true, "0", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &snapshotAttachService{snapshot: agentsessions.AttachSnapshot{OldestOffset: tc.oldest, NextOffset: tc.next}, missing: tc.missing}
			h := NewHandler(Deps{Service: svc})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/s/attach?since_seq="+tc.request, nil))
			response := w.Result()
			defer response.Body.Close()
			if tc.missing {
				if svc.callback || response.Header.Get("X-Tether-Attach-Oldest-Offset") != "" || response.StatusCode != http.StatusConflict {
					t.Fatalf("invented unknown window %+v", response)
				}
				return
			}
			if response.Header.Get("X-Tether-Attach-Evicted") != tc.evicted || response.Header.Get("X-Tether-Attach-Cursor") != "byte-offset" {
				t.Fatalf("headers %+v", response.Header)
			}
			if response.Header.Get("X-Tether-Attach-Oldest-Offset") == "" || response.Header.Get("X-Tether-Attach-Next-Offset") == "" || w.Body.String() != "retained bytes" {
				t.Fatalf("window absent before bytes %+v %s", response.Header, w.Body.String())
			}
		})
	}
}

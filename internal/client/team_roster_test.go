package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

func TestRosterClientEscapesSelectorAndNeverClaimsCaller(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Query().Get("run_id") != "run&caller=other" || r.URL.Query().Has("caller") || r.Header.Get("Idempotency-Key") != "" {
			t.Error("unexpected request", r.Method, r.URL)
		}
		_, _ = w.Write([]byte(`{"runs":[]}`))
	}))
	defer server.Close()
	c := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken(""))
	_, err := c.TeamRoster(context.Background(), teamsvc.RosterRequest{RunID: "run&caller=other"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.TeamRoster(context.Background(), teamsvc.RosterRequest{Limit: 101})
	var failure *TeamError
	if !errors.As(err, &failure) || failure.Code != "invalid_request" || calls != 1 {
		t.Fatal(err, calls)
	}
}

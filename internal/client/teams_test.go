package client

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/api"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamMiddlewareStatusErrors(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, code string
		want       int
	}{
		{401, `{"error":{"code":"unauthorized","message":"private cause"}}`, "unauthenticated", 401},
		{403, `{"error":{"code":"forbidden","message":"private cause"}}`, "denied", 403},
		{405, "method forbidden private cause", "method_not_allowed", 405},
		{503, `{"error":{"code":"identity_unavailable","message":"private cause"}}`, "unavailable", 503},
		{502, "private cause", "unavailable", 503},
		{500, `{"error":{"code":"internal_error","message":"private cause"}}`, "internal_error", 500},
	} {
		t.Run(tc.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := New("tcp:"+strings.TrimPrefix(server.URL, "http://"), WithToken(""))
			_, err := c.Team(context.Background(), "dissolve", "key", api.TeamRequest{RunID: "run"})
			var failure *TeamError
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Status != tc.want || strings.Contains(err.Error(), "private") {
				t.Fatal(err, tc.code, tc.want)
			}
		})
	}
}

func TestTeamClientRejectsInvalidKeys(t *testing.T) {
	for _, key := range []string{" key", "key ", "key\n", "key\x00", string([]byte{0xff})} {
		c := New("tcp:127.0.0.1:1", WithToken(""))
		_, err := c.Team(context.Background(), "dissolve", key, api.TeamRequest{RunID: "run"})
		var failure *TeamError
		if !errors.As(err, &failure) || failure.Code != "invalid_request" {
			t.Fatal(key, err)
		}
	}
}

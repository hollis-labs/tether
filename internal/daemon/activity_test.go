package daemon

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

func TestActivityRelayRequiresReadScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	server := &Server{Identity: ids, Workers: workerhub.New(db.DB()), Config: Config{RemoteListener: RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}
	defer server.CloseIdentityAudit()
	for _, tc := range []struct {
		scope string
		want  int
	}{{"", 401}, {"operate", 403}, {"read", 200}} {
		t.Run(tc.scope, func(t *testing.T) {
			req := httptest.NewRequest("GET", "http://worker.test/workers/activity", nil)
			req.Header.Set("Tether-Protocol", "1")
			if tc.scope != "" {
				expires := time.Now().Add(time.Hour)
				token, err := ids.Mint(context.Background(), identity.Principal{ID: "activity-" + tc.scope, Kind: "device", Scopes: []string{tc.scope}, ExpiresAt: &expires})
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res := httptest.NewRecorder()
			server.RemoteHandler("worker.test").ServeHTTP(res, req)
			if res.Code != tc.want {
				t.Fatalf("HTTP %d, want %d: %s", res.Code, tc.want, res.Body.String())
			}
			if tc.want == 200 && !strings.Contains(res.Body.String(), `"reports":[]`) {
				t.Fatalf("missing empty fleet observation: %s", res.Body.String())
			}
		})
	}
}

package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/store"
)

func TestChannelsReachableThroughDaemon(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "channel-routes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	svc := channels.New(db, nil)
	server := &Server{Channels: svc}
	handler := server.Handler()
	req := httptest.NewRequest("GET", "/channels/ops/messages?as=msg://user/local/observer", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("daemon channel history = %d: %s", w.Code, w.Body.String())
	}
	var page channels.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Name != "ops" || page.Address != "msg://service/local/channel/ops" {
		t.Fatalf("history=%+v", page)
	}
	req = httptest.NewRequest("GET", "/channels?as=msg://user/local/observer", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("daemon channel list = %d: %s", w.Code, w.Body.String())
	}
}

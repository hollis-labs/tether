package mcptransport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func TestDaemonRefAttacherLeavesProvenanceToDaemon(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.SessionRefAttachResponse{Inserted: true})
	}))
	defer server.Close()
	dc := client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken(""))
	if err := (daemonRefAttacher{client: dc}).AttachSessionRef(context.Background(), "s", "torque_task", "CW-1", "uri", "read", "proxy", "parent"); err != nil {
		t.Fatal(err)
	}
	if _, asserted := got["source"]; asserted {
		t.Fatal("transport sent caller-asserted source")
	}
	if got["ref_id"] != "CW-1" || got["parent_item_id"] != "parent" {
		t.Fatalf("capture payload changed: %v", got)
	}
}

package workerhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func TestOutpostGrantRequiresExactWorkerID(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.DB().ExecContext(context.Background(), `INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES('worker_a','ambiguous','device_a','linux','{}','',''),('worker_b','ambiguous','device_b','linux','{}','','')`); err != nil {
		t.Fatal(err)
	}
	h := New(db.DB())
	urn := "msg://agent/agent-mux/probe"
	request := func(worker string, remove bool) int {
		t.Helper()
		body := `{"worker":"` + worker + `","urn":"` + urn + `","remove":false}`
		if remove {
			body = strings.Replace(body, `false`, `true`, 1)
		}
		r := httptest.NewRequest(http.MethodPost, "/workers/grants", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.GrantsHTTP(w, r)
		return w.Code
	}
	if code := request("ambiguous", false); code != 400 {
		t.Fatal("label grant accepted", code)
	}
	if code := request("worker_a", false); code != 200 {
		t.Fatal("exact grant refused", code)
	}
	var count int
	if err = db.DB().QueryRow(`SELECT COUNT(*) FROM worker_mail_grants WHERE worker_id='worker_a' AND urn=?`, urn).Scan(&count); err != nil || count != 1 {
		t.Fatal("grant not persisted", count, err)
	}
	if code := request("worker_a", true); code != 200 {
		t.Fatal("revoke refused", code)
	}
	if err = db.DB().QueryRow(`SELECT COUNT(*) FROM worker_mail_grants`).Scan(&count); err != nil || count != 0 {
		t.Fatal("grant not removed", count, err)
	}
}

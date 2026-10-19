package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
	"github.com/hollis-labs/tether/internal/events"
)

func TestMessageEventsActivationFencesExistingTransitionsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	names, err := fs.ReadDir(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	old := fstest.MapFS{}
	for _, entry := range names {
		if entry.Name() == "0091_message_lifecycle.sql" {
			continue
		}
		raw, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		old[entry.Name()] = &fstest.MapFile{Data: raw}
	}
	if _, err = migrateFS(db, old); err != nil {
		t.Fatal(err)
	}
	if err = delivery.ApplySQLiteSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	before, err := delivery.NewSQLiteStore(db).Enqueue(context.Background(), delivery.EnqueueRequest{From: from, Recipients: []delivery.RecipientTarget{{Address: to}}, Kind: messaging.MsgKindNotice, Payload: []byte(`"OLD-BODY"`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO messages(id,kind,channel,from_urn,to_urn,payload,created_at,delivery_id) VALUES (?,'notice','',?,?,?, ?,?)`, before.Message.ID, from.URN(), to.URN(), `"OLD-BODY"`, time.Now().UTC().Format(time.RFC3339Nano), before.Deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	if n, err := st.DrainMessageEvents(context.Background(), bus); err != nil || n != 0 {
		t.Fatalf("historical transitions replayed: %d %v", n, err)
	}
	after, err := st.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	// A restart must preserve the first activation fence, including unprojected
	// NEW transitions committed between activation and the restart.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bus = events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	if _, err = st.DrainMessageEvents(context.Background(), bus); err != nil {
		t.Fatal(err)
	}
	rows, err := st.EventsSince(0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("new events=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if !strings.Contains(row.PayloadJSON, after.ID) || strings.Contains(row.PayloadJSON, string(before.Message.ID)) {
			t.Fatal("activation boundary lost")
		}
	}
	oldMail, err := st.MessagingStore().Get(context.Background(), string(before.Message.ID))
	if err != nil || string(oldMail.Payload) != `"OLD-BODY"` {
		t.Fatalf("history changed: payload=%q err=%v", oldMail.Payload, err)
	}
}

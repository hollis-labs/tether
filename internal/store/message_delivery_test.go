package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

func TestDeliveryHandoffFirstWinsAndReadImpliesDelivery(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	ms := st.MessagingStore()
	for _, path := range []string{"watch_stream", "list", "get", "inbox", "wake_hub", "wake_outpost", "read_implied"} {
		t.Run(path, func(t *testing.T) {
			env, err := ms.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
			if err != nil {
				t.Fatal(err)
			}
			marker := ms.(DeliveryMarker)
			if err := marker.MarkDelivered(ctx, env.ID, from, "get", "sender"); !errors.Is(err, ErrWrongRecipient) {
				t.Fatalf("sender handoff: %v", err)
			}
			switch path {
			case "read_implied":
				if err := ms.MarkRead(ctx, env.ID, to); err != nil {
					t.Fatal(err)
				}
			case "inbox":
				if _, err := ms.Inbox(ctx, to, messaging.Filter{}); err != nil {
					t.Fatal(err)
				}
			default:
				var wg sync.WaitGroup
				for range 6 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := marker.MarkDelivered(ctx, env.ID, to, path, "s1"); err != nil {
							t.Error(err)
						}
					}()
				}
				wg.Wait()
			}
			if err := ms.MarkRead(ctx, env.ID, to); err != nil {
				t.Fatal(err)
			}
			if err := marker.MarkDelivered(ctx, env.ID, to, "get", "s2"); err != nil {
				t.Fatal(err)
			}
			var delivered, read, gotPath string
			if err := st.db.QueryRow(`SELECT delivered_at, read_at, delivery_path FROM messages WHERE id=?`, env.ID).Scan(&delivered, &read, &gotPath); err != nil {
				t.Fatal(err)
			}
			if parseMsgTime(delivered).After(parseMsgTime(read)) || gotPath != path {
				t.Fatalf("delivery=%s read=%s path=%s", delivered, read, gotPath)
			}
			var count int
			var eventPath string
			if err := st.db.QueryRow(`SELECT count(*), json_extract(payload_json,'$.delivery.path') FROM message_event_journal WHERE kind='message.delivered' AND json_extract(payload_json,'$.message_id')=?`, env.ID).Scan(&count, &eventPath); err != nil {
				t.Fatal(err)
			}
			if count != 1 || eventPath != path {
				t.Fatalf("events=%d path=%s", count, eventPath)
			}
		})
	}
}

func TestDeliveryMigrationBackfillsReadWithoutDeliveryRow(t *testing.T) {
	db := openTempDB(t)
	defer db.Close()
	files, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations(files)
	if err != nil {
		t.Fatal(err)
	}
	before := fstest.MapFS{}
	for _, m := range migrations {
		if m.version < 113 {
			before[fmt.Sprintf("%04d_%s.sql", m.version, m.name)] = &fstest.MapFile{Data: []byte(m.sql)}
		}
	}
	if _, err := migrateFS(db, before); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO messages(id,kind,from_urn,to_urn,created_at,read_at) VALUES ('old','notice','msg://agent/test/a','msg://agent/test/b','2026-01-01T00:00:00Z','2026-01-01T01:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := migrateFS(db, files); err != nil {
			t.Fatal(err)
		}
	}
	var delivered, read, path string
	if err := db.QueryRow(`SELECT delivered_at,read_at,delivery_path FROM messages WHERE id='old'`).Scan(&delivered, &read, &path); err != nil {
		t.Fatal(err)
	}
	if delivered != read || path != "read_implied" {
		t.Fatalf("%s %s %s", delivered, read, path)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM message_event_journal WHERE kind='message.delivered'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

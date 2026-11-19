package chatwire

import (
	"context"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/agent/chat/sqlitestore"
	"github.com/hollis-labs/libs/core/store"
)

// DB is the chat database: one SQLite file under the daemon's state directory holding the conversation event log
// (libs/agent/chat/sqlitestore, which passes streamhub's log conformance suite) and the Control's records.
//
// It is its own file, not Tether's state.db: every streamed delta is a commit, and the daemon's store serves every
// other route through one writer connection. The Control's rows sit beside the log so a conversation's chat state is
// in one place.
type DB struct {
	// Store is the open database.
	Store *store.Store
	// Log is the event log over Store. Only its streamhub.Log side is used; its built-in chat.Control is not.
	Log *sqlitestore.Store
}

// OpenDB opens or creates the chat database at path and applies both schemas.
func OpenDB(ctx context.Context, path string) (*DB, error) {
	db, err := store.Open(ctx, store.Config{
		Driver: store.DriverSQLite,
		// NORMAL in WAL mode keeps each delta's commit safe across a process crash without an fsync per token
		// (sqlitestore.WithSynchronous).
		SQLite: store.SQLiteConfig{Path: path, CreateParentDir: true, Synchronous: "NORMAL"},
	})
	if err != nil {
		return nil, fmt.Errorf("chatwire: open %s: %w", path, err)
	}
	log, err := sqlitestore.New(ctx, db)
	if err == nil {
		err = applySchema(ctx, db)
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("chatwire: schema: %w", err)
	}
	return &DB{Store: db, Log: log}, nil
}

// Close closes the database.
func (d *DB) Close() error {
	return errors.Join(d.Log.Close(), d.Store.Close())
}

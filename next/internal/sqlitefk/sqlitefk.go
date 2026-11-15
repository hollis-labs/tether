// Package sqlitefk keeps SQLite foreign key enforcement off on every connection the process opens.
//
// The old store never enabled foreign keys, so its 48 declared references (some with ON DELETE CASCADE or
// SET NULL) were never enforced, and live data holds rows that would violate them. go-store opens SQLite
// with foreign_keys on and has no option to turn it off. The port keeps today's behavior on both dialects
// (the Postgres baseline declares no foreign keys): deletes never cascade, and orphan rows are data,
// copied as they are. Enforcing foreign keys is a separate decision, recorded in docs/conversion-notes.md.
//
// Importing the package registers a connection hook with the modernc driver. The driver runs it after the
// DSN pragmas, so it overrides go-store's foreign_keys(1). The hook is process-wide, which matches the
// old process. Remove the package when go-store and the database module offer the option (Substrate gap
// CW-20261115-0012).
package sqlitefk

import (
	"context"

	"modernc.org/sqlite"
)

func init() {
	sqlite.RegisterConnectionHook(func(c sqlite.ExecQuerierContext, _ string) error {
		_, err := c.ExecContext(context.Background(), "PRAGMA foreign_keys = OFF", nil)
		return err
	})
}

// Package migrations embeds Tether's one goose migration set (port plan section 3). It holds no .sql
// file until slice S1 adds the squashed schema; until then Register registers nothing, because the
// database module refuses a source with no migrations and a placeholder version would be one more
// thing S1 has to reconcile. No per-version ladders from the old tree belong here.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed README.md
var fsys embed.FS

// FS returns the migration files and their root directory.
func FS() (fs.FS, string) { return fsys, "." }

// Empty reports whether the set holds no migration yet.
func Empty() bool {
	m, err := fs.Glob(fsys, "*.sql")
	return err != nil || len(m) == 0
}

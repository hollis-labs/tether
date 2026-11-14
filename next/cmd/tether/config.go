package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/module"

	"github.com/hollis-labs/tether/next/internal/datadir"
	"github.com/hollis-labs/tether/next/internal/legacyguard"
	"github.com/hollis-labs/tether/next/internal/settings"
)

// databaseFile is the database's name inside the data directory.
const databaseFile = "tether.db"

// bootOptions are the app's two boot hooks.
//
// AfterConfig runs once the config module has applied the SUBSTRATE_CONFIG override: it refuses a bad
// tether: block, resolves the data directory (TETHER_DATA_DIR), refuses one that is the old data root
// (read-only, before anything is created or chmod'ed), secures it, and writes the absolute database
// path into app.Config, which every later module reads.
//
// Before["database"] runs immediately before the database module opens (and migrates) the file. It
// reads the path from the same config the module will read, so it needs no copy of the merge rule, and
// refuses an old Tether database (legacyguard).
func bootOptions(getenv func(string) string) boot.Options {
	return boot.Options{
		AfterConfig: func(_ context.Context, app *module.App) error {
			if _, err := settings.Parse(app.Config[settings.Key]); err != nil {
				return err
			}
			dir, err := datadir.Resolve(getenv)
			if err != nil {
				return err
			}
			// Read-only, and before Prepare: Prepare creates and chmods, which must never touch the
			// old data root.
			if err := dir.RefuseOldRoot(); err != nil {
				return err
			}
			if err := dir.Prepare(); err != nil {
				return fmt.Errorf("prepare the data directory %s: %w", dir, err)
			}
			db, _ := app.Config["database"].(map[string]any)
			if db == nil {
				db = map[string]any{}
				app.Config["database"] = db
			}
			if driver, _ := db["driver"].(string); driver != "" && driver != "sqlite" {
				return fmt.Errorf("database.driver: Tether is SQLite only, got %q", driver)
			}
			sq, _ := db["sqlite"].(map[string]any)
			if sq == nil {
				sq = map[string]any{}
				db["sqlite"] = sq
			}
			p, _ := sq["path"].(string)
			if p, err = dir.Anchor(databaseFile, p); err != nil {
				return fmt.Errorf("database.sqlite.path: %w", err)
			}
			sq["path"] = p
			return nil
		},
		Before: map[string]boot.Hook{
			"database": func(_ context.Context, app *module.App) error {
				db, _ := app.Config["database"].(map[string]any)
				sq, _ := db["sqlite"].(map[string]any)
				p, _ := sq["path"].(string)
				if p == "" {
					return fmt.Errorf("database.sqlite.path is empty after the data directory was resolved")
				}
				p = filepath.Clean(p)
				if err := legacyguard.Check(p); err != nil {
					return err
				}
				dir, err := datadir.Resolve(getenv)
				if err != nil {
					return err
				}
				return dir.PrivateFile(p)
			},
		},
	}
}

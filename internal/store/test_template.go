package store

// UseMigratedTemplate is for a test package's TestMain, never for production
// code. After it, Open starts every new database from one migrated empty image
// (built on the first Open) instead of replaying all migrations.
//
// Why: under -race, Open costs ~1.8s (SQLite re-parses the growing schema after
// each migration statement through the race detector's pointer checks) against
// ~0.09s without it, and the test packages open hundreds of databases. Seeding
// from a template took internal/store's race run from 15m43s to 2m.
//
// The seam is thin: useTemplate is false unless this is called, so Open is
// unchanged in production, and Open only copies the template to a path that does
// not exist yet, never over an existing file. Call it once, before m.Run: the flag
// is read without a lock afterwards.
func UseMigratedTemplate() { useTemplate = true }

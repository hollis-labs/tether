// Package migrate moves the old store's data into the new one (port plan section 7). Slices S1 and S1b
// add its schema tests and the first acceptance check:
//   - schema_test.go proves migrations/sqlite/00001_schema.sql equal to the old tree's migrations
//     0001-0101 (vendored under testdata/oldmigrations) and regenerates it with -update;
//   - postgres_test.go proves the Postgres set equivalent to the SQLite one, and parity_test.go runs the
//     schema's triggers, checks and defaults on both dialects;
//   - VerifyMessagesSeq is the messages.seq acceptance that migrate-from runs after its copy.
//
// migrate-from itself lands in slice S12.
package migrate

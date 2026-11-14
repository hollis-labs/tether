// Package migrate moves the old store's data into the new one (port plan section 7). Slice S1 adds
// only its schema test: schema_test.go proves migrations/sqlite/00001_schema.sql equal to the old
// tree's migrations 0001-0101 (vendored under testdata/oldmigrations) and regenerates it with
// -update. migrate-from itself lands in slice S12.
package migrate

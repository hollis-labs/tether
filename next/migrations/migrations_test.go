package migrations

import "testing"

// TestEmptyUntilS1 pins today's state; S1 changes it when it adds the squashed schema.
func TestEmptyUntilS1(t *testing.T) {
	if !Empty() {
		t.Fatal("the migration set gained a file: remove this test in the slice that adds the schema")
	}
}

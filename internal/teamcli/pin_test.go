package teamcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pinDefinition = `---
schema_version: "2"
definition_id: pin-agent
revision: r1
name: pin-agent
description: Pin test.
behavior:
  purpose: Test.
requirements: {}
harness_profile:
  context: {}
  permissions:
    profile: default
continuity:
  mode: ephemeral
---
Body.
`

func TestPinOf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.md")
	if err := os.WriteFile(path, []byte(pinDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	id, rev, digest, err := PinOf(path)
	if err != nil || id != "pin-agent" || rev != "r1" || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("PinOf = %q %q %q %v", id, rev, digest, err)
	}
	if err := os.WriteFile(path, []byte("not a definition"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := PinOf(path); err == nil {
		t.Fatal("invalid definition accepted")
	}
}

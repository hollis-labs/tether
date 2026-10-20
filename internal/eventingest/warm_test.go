package eventingest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"
)

func TestWarmCompilesTheSchemaOnce(t *testing.T) {
	if err := warm(); err != nil {
		t.Fatal(err)
	}
	// After the warm-up a real check is quick: the schema is already compiled.
	start := time.Now()
	_, rej := ingest.CheckItem("torque", json.RawMessage(`{"id":"x"}`), ingest.Options{})
	if rej == nil || rej.Code != ingest.CodeSchemaInvalid {
		t.Fatalf("%v", rej)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("check after warm-up took %s; the schema was not precompiled", d)
	}
}

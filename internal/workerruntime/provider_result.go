package workerruntime

import (
	"encoding/json"
	"io"
)

// The hub's durable output writer consumes this result envelope; native
// protocol records are also retained in the shim journal.
func writeProviderResult(out io.Writer, provider, id, text string, failed bool) error {
	return json.NewEncoder(out).Encode(map[string]any{"type": "result", "provider": provider, "uuid": id, "result": text, "is_error": failed})
}

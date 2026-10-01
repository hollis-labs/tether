package mcpadapter

import (
	"fmt"

	"github.com/hollis-labs/tether/internal/config"
)

// A remote server cannot inherit the planted proxy's read-only mounts. Even a
// loopback server runs in the host namespace and may offer arbitrary writes or
// process launches. Until daemon-side authorization can enforce equivalent
// protection, do not grant its tools to a proxy carrying protected paths.
func confineUpstreamTransport(entry config.MCPServerEntry, enabled bool) error {
	if enabled && (entry.Transport == "http" || entry.Transport == "sse") && !entry.AllowUnconfinedRemote {
		return fmt.Errorf("upstream %q: %s upstream cannot be confined locally; use stdio or explicitly set allow_unconfined_remote: true in its catalog entry", entry.ID, entry.Transport)
	}
	return nil
}

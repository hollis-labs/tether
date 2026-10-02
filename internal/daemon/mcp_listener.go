package daemon

import (
	"fmt"
	"net"

	"github.com/hollis-labs/tether/internal/identity"
)

// Check the address actually bound, so a localhost resolution cannot bypass
// the textual preflight. The composition root also checks verifier readiness.
func validateMCPListenerAddress(addr net.Addr, mode identity.Mode, authenticated bool) error {
	if tcp, ok := addr.(*net.TCPAddr); ok && !tcp.IP.IsLoopback() {
		if mode != identity.Enforce || !authenticated {
			return fmt.Errorf("non-loopback MCP listener requires identity.mode enforce and a verifier")
		}
	}
	return nil
}

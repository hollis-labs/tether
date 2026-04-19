// Package daemon implements the long-lived muxd process that owns running
// sessions across CLI invocations. It exposes a small HTTP API (v0.0.2 ships
// only /health; Sprint v002-s05 adds the full surface) served over either a
// Unix domain socket or loopback TCP per the scheme-prefixed listen_addr
// convention established by ADR 0002.
package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// Listener opens the transport described by addr. Supported schemes:
//
//	unix:/absolute/path   → Unix domain socket (parent dirs must exist)
//	tcp:host:port         → loopback TCP, e.g. tcp:127.0.0.1:7180
//
// The addr string is taken verbatim; the caller is responsible for any
// path expansion (use config.Expand) and for cleaning up a stale socket
// file left over from a previous crashed daemon.
func Listener(addr string) (net.Listener, error) {
	switch {
	case strings.HasPrefix(addr, "unix:"):
		path := strings.TrimPrefix(addr, "unix:")
		if path == "" {
			return nil, fmt.Errorf("unix listen_addr missing path")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create socket dir: %w", err)
		}
		// Best-effort stale-socket removal: net.Listen("unix") refuses to
		// bind if the file exists. We only remove if the file is a socket
		// (mode&ModeSocket != 0) so a surprise regular file aborts instead.
		if err := removeStaleSocket(path); err != nil {
			return nil, err
		}
		return net.Listen("unix", path)
	case strings.HasPrefix(addr, "tcp:"):
		hostPort := strings.TrimPrefix(addr, "tcp:")
		if hostPort == "" {
			return nil, fmt.Errorf("tcp listen_addr missing host:port")
		}
		return net.Listen("tcp", hostPort)
	default:
		return nil, fmt.Errorf("unsupported listen_addr scheme in %q (expect unix:/path or tcp:host:port)", addr)
	}
}

// SocketPath returns the filesystem path for a unix: listen_addr, or ""
// for tcp addrs.
func SocketPath(addr string) string {
	if strings.HasPrefix(addr, "unix:") {
		return filepath.Clean(strings.TrimPrefix(addr, "unix:"))
	}
	return ""
}

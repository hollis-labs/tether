package daemon

import (
	"net"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestMCPListenerUsesActualBoundAddress(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		addr                   net.Addr
		mode                   identity.Mode
		authenticated, refused bool
	}{
		{"unix", &net.UnixAddr{Name: "temporary.sock", Net: "unix"}, identity.Observe, false, false},
		{"loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, identity.Observe, false, false},
		{"ipv6-loopback", &net.TCPAddr{IP: net.ParseIP("::1")}, identity.Observe, false, false},
		{"resolved-nonloopback", &net.TCPAddr{IP: net.ParseIP("192.0.2.1")}, identity.Observe, true, true},
		{"wildcard-no-auth", &net.TCPAddr{IP: net.IPv4zero}, identity.Enforce, false, true},
		{"enforced", &net.TCPAddr{IP: net.ParseIP("192.0.2.1")}, identity.Enforce, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateMCPListenerAddress(tc.addr, tc.mode, tc.authenticated); (err != nil) != tc.refused {
				t.Fatalf("listener boundary: %v", err)
			}
		})
	}
}

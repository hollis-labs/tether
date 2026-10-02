package mcpadapter

import (
	"io"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestStdioUpstreamDoesNotInheritTetherCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "session-canary")
	t.Setenv("TETHER_MCP_TOKEN", "marker-canary")
	t.Setenv("TETHER_OTHER_TOKEN", "other-canary")
	t.Setenv("TETHER_SESSION", "session-id")
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "inherited", true: "explicit catalog"}[explicit], func(t *testing.T) {
			entry := config.MCPServerEntry{Command: "/bin/sh", Args: []string{"-c", `printf '%s\n' "${TETHER_TOKEN-unset}" "${TETHER_MCP_TOKEN-unset}" "${TETHER_OTHER_TOKEN-unset}" "$TETHER_SESSION"`}}
			want := "unset\nunset\nunset\nsession-id\n"
			if explicit {
				entry.Env = map[string]string{"TETHER_TOKEN": "catalog-token", "TETHER_MCP_TOKEN": "catalog-marker", "TETHER_OTHER_TOKEN": "catalog-other"}
				want = "catalog-token\ncatalog-marker\ncatalog-other\nsession-id\n"
			}
			u, transport, err := spawnStdioUpstream(entry)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = transport.Reader.Close(); _ = transport.Writer.Close() }()
			got, err := io.ReadAll(transport.Reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := u.cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatal("wrong child credential inheritance", strings.TrimSpace(string(got)))
			}
		})
	}
}

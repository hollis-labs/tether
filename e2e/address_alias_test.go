package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
)

// TestAddressAliasMigration drives the CW-20261114-0067 migration through the
// real CLI against a real, isolated daemon: a person's two old addresses (the
// operator principal's and a registry agent row) become aliases of one chosen
// address, and the documented commands behave as docs/messaging.md says. The
// mailbox is synthetic; nothing here reads a real state database or token.
func TestAddressAliasMigration(t *testing.T) {
	d := StartFixtureDaemon(t)
	const (
		canonical = "msg://user/local/chrispian"
		operator  = "msg://user/local/operator"
		sender    = "msg://agent/agent-mux/e2e-sender"
	)
	// The fixture's own operator credential, as the CLI would resolve it.
	operatorClient := client.New(d.SocketAddr, client.WithTokenFile(filepath.Join(d.StateRoot, "run", "operator.token")))
	agentRow := registerAgent(t, operatorClient, "chrispian")

	tether := func(wantOK bool, args ...string) string {
		t.Helper()
		cmd := exec.Command(d.bin, append([]string{"--catalog", d.CatalogDir}, args...)...) //nolint:gosec // G204: our own just-built test binary
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if shown := out.String(); len(shown) > 400 {
			t.Logf("$ tether %s\n(%d bytes)", strings.Join(args, " "), len(shown))
		} else {
			t.Logf("$ tether %s\n%s", strings.Join(args, " "), shown)
		}
		if (err == nil) != wantOK {
			t.Fatalf("tether %s: err=%v, want ok=%v", strings.Join(args, " "), err, wantOK)
		}
		return out.String()
	}
	send := func(to, body string) string {
		t.Helper()
		var sent struct {
			ID string `json:"id"`
			To string `json:"to"`
		}
		out := tether(true, "messages", "send", "--json", "--from", sender, "--to", to, body)
		if err := json.Unmarshal([]byte(out), &sent); err != nil || sent.ID == "" {
			t.Fatalf("send output: %v\n%s", err, out)
		}
		return sent.ID
	}
	type listed struct {
		ID         string  `json:"id"`
		To         string  `json:"to"`
		ReadAt     *string `json:"read_at"`
		ArchivedAt *string `json:"archived_at"`
	}
	list := func(to string) []listed {
		t.Helper()
		var page struct {
			Messages []listed `json:"messages"`
		}
		out := tether(true, "messages", "list", to, "--json", "--include-archived")
		if err := json.Unmarshal([]byte(out), &page); err != nil {
			t.Fatalf("list output: %v\n%s", err, out)
		}
		return page.Messages
	}
	ids := func(msgs []listed) string {
		var out []string
		for _, m := range msgs {
			out = append(out, m.ID)
		}
		return strings.Join(out, ",")
	}

	// The mailbox as it is before the migration: mail under both old
	// addresses, one message read and one archived.
	oldUnread := send(operator, "old, unread")
	oldRead := send(operator, "old, read")
	oldArchived := send(operator, "old, archived")
	oldAgent := send(agentRow, "old, to the agent row")
	tether(true, "messages", "read", oldRead, "--as", operator)
	tether(true, "messages", "archive", oldArchived, "--as", operator)
	if got := list(canonical); len(got) != 0 {
		t.Fatalf("canonical mailbox before the migration: %s", ids(got))
	}

	// Only the operator credential may declare an alias.
	anonymous := exec.Command(d.bin, "--catalog", d.CatalogDir, "--token-file", filepath.Join(d.StateRoot, "no-such-token"), "messages", "alias", "add", operator, canonical) //nolint:gosec // G204: our own just-built test binary
	if out, err := anonymous.CombinedOutput(); err == nil {
		t.Fatalf("alias add without the operator credential succeeded:\n%s", out)
	}
	if out := tether(true, "messages", "alias", "list"); !strings.Contains(out, "no address aliases") {
		t.Fatalf("alias list after a refused add:\n%s", out)
	}

	// The migration: one command per old address.
	if out := tether(true, "messages", "alias", "add", operator, canonical); !strings.Contains(out, "3 stored message(s)") {
		t.Fatalf("alias add (operator) did not report the 3 merged messages:\n%s", out)
	}
	if out := tether(true, "messages", "alias", "add", agentRow, canonical); !strings.Contains(out, "1 stored message(s)") {
		t.Fatalf("alias add (agent row) did not report the 1 merged message:\n%s", out)
	}
	if out := tether(true, "messages", "alias", "list", "--canonical", canonical); !strings.Contains(out, operator+" -> "+canonical) || !strings.Contains(out, agentRow+" -> "+canonical) {
		t.Fatalf("alias list:\n%s", out)
	}
	// A chain is refused.
	tether(false, "messages", "alias", "add", canonical, sender)

	// Acceptance 1 and 2: three sends, three lists.
	viaOperator := send(operator, "new, to the operator address")
	viaAgent := send(agentRow, "new, to the agent row")
	direct := send(canonical, "new, to the canonical address")
	fromCanonical := list(canonical)
	want := strings.Join([]string{direct, viaAgent, viaOperator, oldAgent, oldArchived, oldRead, oldUnread}, ",")
	if ids(fromCanonical) != want {
		t.Fatalf("list %s = %s, want %s", canonical, ids(fromCanonical), want)
	}
	for _, addr := range []string{operator, agentRow} {
		if got := list(addr); ids(got) != want {
			t.Fatalf("list %s = %s, want %s", addr, ids(got), want)
		}
	}
	// Acceptance 3: the old mail is listed with its address and state untouched.
	for _, m := range fromCanonical {
		switch m.ID {
		case oldUnread:
			if m.To != operator || m.ReadAt != nil || m.ArchivedAt != nil {
				t.Fatalf("old unread message changed: %+v", m)
			}
		case oldRead:
			if m.To != operator || m.ReadAt == nil || m.ArchivedAt != nil {
				t.Fatalf("old read message changed: %+v", m)
			}
		case oldArchived:
			if m.To != operator || m.ArchivedAt == nil || m.ReadAt != nil {
				t.Fatalf("old archived message changed: %+v", m)
			}
		case oldAgent:
			if m.To != agentRow || m.ReadAt != nil {
				t.Fatalf("old agent-row message changed: %+v", m)
			}
		default:
			if m.To != canonical {
				t.Fatalf("new message %s stored under %s", m.ID, m.To)
			}
		}
	}

	// Mark read through an old address, on a message stored under the other.
	tether(true, "messages", "read", oldAgent, "--as", operator)
	tether(false, "messages", "read", direct, "--as", sender)

	// Acceptance 6: the registry names the canonical address for every form.
	for _, addr := range []string{canonical, operator, agentRow} {
		if out := tether(true, "registry", "lookup", addr); !strings.Contains(out, "chrispian") || !strings.Contains(out, "canonical address: "+canonical) {
			t.Fatalf("registry lookup %s:\n%s", addr, out)
		}
	}

	// The alias table and the merged view survive a restart.
	d.Stop()
	d.Restart()
	if got := list(operator); ids(got) != want {
		t.Fatalf("list after restart = %s, want %s", ids(got), want)
	}

	// Rollback: removing the aliases restores the separate mailboxes without
	// touching a message. Mail sent during the alias stays where it was stored.
	tether(true, "messages", "alias", "remove", operator)
	tether(true, "messages", "alias", "remove", agentRow)
	if got := list(operator); ids(got) != strings.Join([]string{oldArchived, oldRead, oldUnread}, ",") {
		t.Fatalf("operator mailbox after rollback = %s", ids(got))
	}
	if got := list(canonical); ids(got) != strings.Join([]string{direct, viaAgent, viaOperator}, ",") {
		t.Fatalf("canonical mailbox after rollback = %s", ids(got))
	}
	if _, err := os.Stat(d.StateDBPath()); err != nil {
		t.Fatalf("state db: %v", err)
	}
}

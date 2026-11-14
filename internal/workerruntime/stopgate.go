package workerruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The Stop-hook reply gate (CW-20261114-0013, design v2 section 8). Claude runs `tether-worker hook stop-gate` when a
// turn is about to end. The gate asks the hub (tether_pending_replies over /mcp, with the session's agent token) whether
// a typed message is waiting on this seat's reply. It blocks a stop at most once per message: the ids it already blocked
// on are kept in the session's state file, and a message older than StopGateWindow never blocks (the watchdog owns it,
// CW-20261114-0015). A block names the messages and the tool to answer them; Claude continues the turn with that reason.
// It never blocks a stop that a previous block caused (stop_hook_active), and it fails open: any error, panic, timeout
// or unreachable hub lets the turn end.

// StopGateTimeout bounds the whole hub round trip.
const StopGateTimeout = 8 * time.Second

// StopGateWindow is how recent a pending message must be to block a stop.
const StopGateWindow = 24 * time.Hour

// maxStopGateState bounds the remembered ids (newest kept).
const maxStopGateState = 500

// askPending is the hub round trip; a test replaces it.
var askPending = askPendingReplies

// maxStopGateListed is how many pending messages the reason names.
const maxStopGateListed = 5

type pendingReply struct {
	MessageID string    `json:"message_id"`
	From      string    `json:"from"`
	Kind      string    `json:"kind"`
	Subject   string    `json:"subject,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// RunStopGate reads the Claude Stop hook payload from stdin and writes a block decision to stdout when a typed message
// waits on this seat that has not blocked a stop before. as is the seat's address (TETHER_AGENT_URN); empty lets the
// hub use the token's identity. statePath remembers the ids already blocked on. It never fails Claude: an error or a
// panic is one line on stderr (never the token), nothing on stdout, and the turn ends normally.
func RunStopGate(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, hubURL, tokenFile, as, statePath string) {
	defer func() {
		if p := recover(); p != nil {
			fmt.Fprintf(stderr, "tether-worker stop-gate: internal error (%v); not gating\n", p)
		}
	}()
	var hook struct {
		StopHookActive bool `json:"stop_hook_active"`
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil || json.Unmarshal(raw, &hook) != nil {
		fmt.Fprintln(stderr, "tether-worker stop-gate: unreadable hook payload; not gating")
		return
	}
	if hook.StopHookActive {
		return // this stop follows our own block: let it end, never loop
	}
	ctx, cancel := context.WithTimeout(ctx, StopGateTimeout)
	defer cancel()
	pending, err := askPending(ctx, &HTTPRelay{URL: hubURL, TokenFile: tokenFile}, as)
	if err != nil {
		fmt.Fprintf(stderr, "tether-worker stop-gate: %v; not gating\n", err)
		return
	}
	seen := readStopGateState(statePath)
	cutoff := time.Now().Add(-StopGateWindow)
	var fresh []pendingReply
	for _, p := range pending {
		if seen[p.MessageID] || (!p.CreatedAt.IsZero() && p.CreatedAt.Before(cutoff)) {
			continue
		}
		fresh = append(fresh, p)
	}
	if len(fresh) == 0 {
		return
	}
	// Remember them before blocking, so a message blocks at most one stop even if the write below is the last thing
	// that happens. A state that cannot be written means no block: never nag without a memory.
	if err := writeStopGateState(statePath, seen, fresh); err != nil {
		fmt.Fprintf(stderr, "tether-worker stop-gate: state: %v; not gating\n", err)
		return
	}
	out, err := json.Marshal(map[string]string{"decision": "block", "reason": stopGateReason(fresh)})
	if err != nil {
		return
	}
	_, _ = stdout.Write(append(out, '\n'))
}

// readStopGateState is the set of ids already blocked on (empty when the file is missing or unreadable).
func readStopGateState(path string) map[string]bool {
	seen := map[string]bool{}
	if path == "" {
		return seen
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path the worker planted in this session's hook settings
	if err != nil {
		return seen
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			seen[id] = true
		}
	}
	return seen
}

// writeStopGateState appends the newly blocked ids, keeping the newest maxStopGateState, through a temp file and a
// rename (the agent can write its workspace; a link left at path is replaced, not followed).
func writeStopGateState(path string, seen map[string]bool, blocked []pendingReply) error {
	if path == "" || !filepath.IsAbs(path) {
		return errors.New("no state file")
	}
	ids := readStateLines(path)
	for _, p := range blocked {
		if !seen[p.MessageID] {
			ids = append(ids, p.MessageID)
		}
	}
	if len(ids) > maxStopGateState {
		ids = ids[len(ids)-maxStopGateState:]
	}
	return writeFresh(path, []byte(strings.Join(ids, "\n")+"\n"))
}

func readStateLines(path string) []string {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: see readStopGateState
	if err != nil {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(string(raw), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func stopGateReason(pending []pendingReply) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Before ending this turn: %d typed message(s) wait on your reply.\n", len(pending))
	for i, p := range pending {
		if i == maxStopGateListed {
			fmt.Fprintf(&b, "- ...and %d more (tether_pending_replies lists them)\n", len(pending)-maxStopGateListed)
			break
		}
		line := fmt.Sprintf("- %s %s from %s", p.MessageID, p.Kind, p.From)
		if p.Subject != "" {
			line += ": " + p.Subject
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("Answer each with tether_message_post (in_reply_to=<id>; for a question, decision or approval_request set answer.option). " +
		"If you will not act on one, reply saying so. Then end the turn.")
	return b.String()
}

// askPendingReplies runs one short MCP session against the hub through the relay's own transport (token re-read per
// request, https only, no redirects): initialize, initialized, then tools/call tether_pending_replies, in order.
func askPendingReplies(ctx context.Context, r *HTTPRelay, as string) ([]pendingReply, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	r.out = &out
	// The relay may open its standalone server stream after initialized; end it with this call.
	ctx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		r.streamWG.Wait()
	}()
	args := map[string]any{}
	if as != "" {
		args["as"] = as
	}
	steps := []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "tether-stop-gate", "version": "1"}}},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "tether_pending_replies", "arguments": args}},
	}
	for _, step := range steps {
		msg, err := json.Marshal(step)
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, errors.New("hub did not answer in time")
		}
		r.forward(ctx, msg)
	}
	r.closeSession()
	return parsePendingReplies(out.Bytes())
}

// parsePendingReplies finds the tools/call (id 2) response among the relay's output lines.
func parsePendingReplies(lines []byte) ([]pendingReply, error) {
	sc := bufio.NewScanner(bytes.NewReader(lines))
	sc.Buffer(make([]byte, 0, 64<<10), maxRelayLine)
	for sc.Scan() {
		var resp struct {
			ID     json.RawMessage           `json:"id"`
			Error  *struct{ Message string } `json:"error"`
			Result *struct {
				IsError bool `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &resp) != nil || string(resp.ID) != "2" {
			continue
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("hub refused tether_pending_replies: %s", resp.Error.Message)
		}
		if resp.Result == nil || resp.Result.IsError || len(resp.Result.Content) == 0 {
			return nil, errors.New("hub returned no pending-replies result")
		}
		var body struct {
			Pending []pendingReply `json:"pending"`
		}
		if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &body); err != nil {
			return nil, fmt.Errorf("pending-replies result: %w", err)
		}
		return body.Pending, nil
	}
	return nil, errors.New("hub did not answer tether_pending_replies")
}

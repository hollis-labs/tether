package eventstamp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/hollis-labs/libs/security/redact"
	"github.com/hollis-labs/tether/internal/events"
)

// Seats is the directory the seat stage reads: the catalog's agents. It is held
// in memory, so the stage never touches the database.
type Seats interface {
	// Cell is the cell of the catalog agent with this id, or "".
	Cell(agentID string) string
	// AgentForURN is the catalog agent id whose registry identity is urn, or "".
	AgentForURN(urn string) string
}

// Seat stamps scope.cell and the one-line summary on every event
// (CW-20261114-0019). It runs after the row stage, so a session's logical agent
// is already known. The cell comes from the catalog agent the event belongs to;
// the summary, when the producer gave none, is built from the kind and a few
// low-risk payload fields (names, ids, states, counts), never from content. A
// producer's own summary is kept but cleaned the same way: one line, redacted,
// at most events.SummaryMax runes.
//
// It is TxSafe: the directory is the in-memory catalog.
type Seat struct {
	seats Seats
}

var _ events.Stage = (*Seat)(nil)

// NewSeat returns the stage. A nil directory stamps summaries only.
func NewSeat(seats Seats) *Seat { return &Seat{seats: seats} }

// TxSafe reports that Stamp never touches the database.
func (*Seat) TxSafe() bool { return true }

// Stamp implements events.Stage.
func (s *Seat) Stamp(_ context.Context, e *events.Event) error {
	var p map[string]any
	if e.Kind == "session.output" {
		// Raw output is the hot path and its payload is the provider's bytes:
		// read only the stream name.
		var out struct {
			Stream string `json:"stream"`
		}
		_ = json.Unmarshal([]byte(e.PayloadJSON), &out)
		p = map[string]any{"stream": out.Stream}
	} else if e.PayloadJSON != "" {
		_ = json.Unmarshal([]byte(e.PayloadJSON), &p)
	}
	if s.seats != nil && e.Envelope.Dims.Cell == "" {
		if agent := s.agentOf(e, p); agent != "" {
			e.Envelope.Dims.Cell = s.seats.Cell(agent)
		}
	}
	summary := e.Envelope.Summary
	if summary == "" {
		summary = Summarize(e.Kind, p, s.handle)
	}
	e.Envelope.Summary = CleanSummary(summary)
	return nil
}

// agentOf is the catalog agent an event belongs to: its logical agent, else its
// agent URN, else for mail the party it is about (the sender of a send, the
// recipient of everything after).
func (s *Seat) agentOf(e *events.Event, p map[string]any) string {
	if id := e.Envelope.Dims.LogicalAgentID; id != "" {
		return id
	}
	if e.LogicalAgentID != "" {
		return e.LogicalAgentID
	}
	if urn := e.Envelope.Dims.AgentURN; urn != "" {
		return s.seats.AgentForURN(urn)
	}
	if strings.HasPrefix(e.Kind, "message.") {
		if mailSender(e.Kind) {
			return s.seats.AgentForURN(str(p, "from"))
		}
		return s.seats.AgentForURN(first(p, "to"))
	}
	return ""
}

func mailSender(kind string) bool {
	switch kind {
	case "message.sent", "message.queued", "message.redriven", "message.canceled":
		return true
	}
	return false
}

// handle names an address: @<catalog agent id> when the URN is a catalog
// agent's identity, else @<last path segment>.
func (s *Seat) handle(urn string) string {
	if urn == "" {
		return ""
	}
	if s.seats != nil {
		if id := s.seats.AgentForURN(urn); id != "" {
			return "@" + id
		}
	}
	return Handle(urn)
}

// Handle is the fallback @name for an address: its last path segment.
func Handle(urn string) string {
	if i := strings.LastIndexByte(urn, '/'); i >= 0 && i < len(urn)-1 {
		urn = urn[i+1:]
	}
	return "@" + urn
}

// CleanSummary makes s one redacted line of at most events.SummaryMax runes.
func CleanSummary(s string) string {
	if s == "" {
		return ""
	}
	s = redact.Text(s)
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	if r := []rune(s); len(r) > events.SummaryMax {
		s = string(r[:events.SummaryMax-1]) + "…"
	}
	return s
}

// Summarize builds a summary from the kind and low-risk payload fields. handle
// names an address. Unknown kinds get the kind plus whatever from/to, state,
// tool and reason fields the payload has.
func Summarize(kind string, p map[string]any, handle func(string) string) string {
	if handle == nil {
		handle = Handle
	}
	switch {
	case strings.HasPrefix(kind, "message."):
		return mailSummary(kind, p, handle)
	case kind == "tool.call.started" || kind == "tool_call_start":
		return join("tool", toolName(p), "started")
	case kind == "tool.call.finished" || kind == "tool_call_end":
		outcome := "finished"
		if ok, set := p["ok"].(bool); set && !ok {
			outcome = "failed"
			if c := str(p, "error_class"); c != "" {
				outcome += " (" + c + ")"
			}
		}
		if ms := num(p, "duration_ms"); ms > 0 {
			outcome += fmt.Sprintf(" in %dms", ms)
		}
		return join("tool", toolName(p), outcome)
	case kind == "session.output":
		return join("output on", str(p, "stream"))
	case kind == "session.compacted":
		out := "context compacted"
		if t := str(p, "trigger"); t != "" {
			out += " (" + t + ")"
		}
		if n := num(p, "pre_tokens"); n > 0 {
			out += fmt.Sprintf(", %d tokens before", n)
		}
		return out
	case kind == "session.cleared":
		return "context cleared"
	case kind == "mail.triaged":
		return join(str(p, "handle"), "mail from", handleOf(str(p, "from")), "triaged", str(p, "class"), paren(str(p, "reason")))
	case kind == "seat.gate_blocked":
		return join(str(p, "handle"), fmt.Sprintf("stop gate blocked the turn on %d pending message(s)", num(p, "count")))
	case kind == "seat.status":
		out := join(str(p, "handle"), str(p, "state"))
		if r := str(p, "reason"); r != "" {
			out += ": " + r
		}
		if o := str(p, "owner"); o != "" {
			out += " (owner " + o + ")"
		}
		return out
	case kind == "seat.waiting":
		return join("waiting for", str(p, "waiting_for"))
	case kind == "seat.unblocked":
		if w := str(p, "waiting_for"); w != "" {
			return "unblocked (" + w + ")"
		}
		return "unblocked"
	case kind == "session.request_status":
		if b, _ := p["terminal"].(bool); b {
			return "turn ended; open requests closed"
		}
		state := "resolved"
		if b, _ := p["open"].(bool); b {
			state = "opened"
		}
		return join(str(p, "request_kind"), "request", state)
	case kind == "wake.attempted" || kind == "wake.outcome":
		out := join("wake", handle(str(p, "target")))
		if d, set := p["delivered"].(bool); set {
			if d {
				return out + ": delivered"
			}
			return join(out+": not delivered", paren(str(p, "reason")))
		}
		return out
	}
	return join(kind, transition(p), toolName(p), paren(str(p, "reason")))
}

func mailSummary(kind string, p map[string]any, handle func(string) string) string {
	verb := strings.TrimPrefix(kind, "message.")
	what := str(p, "envelope_kind") // a typed message (CW-20261114-0013)
	if what == "" {
		what = str(p, "msg_kind")
	}
	if what == "" {
		what = "message"
	}
	from, to := handle(str(p, "from")), handle(first(p, "to"))
	if g := str(p, "group"); g != "" {
		to = handle(g)
	}
	switch verb {
	case "sent", "queued", "redriven", "canceled":
		return join(from, verb, what, "to", to)
	case "read", "consumed", "delivered", "archived", "purged":
		return join(to, verb, what, "from", from)
	case "failed", "dead_lettered", "wake_deferred":
		return join(what, "from", from, "to", to, verb, paren(str(p, "reason")))
	}
	return join(kind, what, "from", from, "to", to)
}

func toolName(p map[string]any) string {
	t := str(p, "tool")
	if t == "" {
		t = str(p, "tool_name")
	}
	if srv := str(p, "server"); srv != "" && t != "" {
		return srv + "." + t
	}
	return t
}

func transition(p map[string]any) string {
	from, to := str(p, "from"), str(p, "to")
	switch {
	case from != "" && to != "":
		return from + " → " + to
	case to != "":
		return "→ " + to
	}
	for _, k := range []string{"state", "status"} {
		if v := str(p, k); v != "" {
			return v
		}
	}
	return ""
}

func str(p map[string]any, k string) string {
	v, _ := p[k].(string)
	return v
}

func first(p map[string]any, k string) string {
	switch v := p[k].(type) {
	case string:
		return v
	case []any:
		if len(v) > 0 {
			s, _ := v[0].(string)
			return s
		}
	}
	return ""
}

func num(p map[string]any, k string) int64 {
	v, _ := p[k].(float64)
	return int64(v)
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

func join(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

func handleOf(urn string) string {
	if urn == "" {
		return ""
	}
	return Handle(urn)
}

package eventstamp

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/events"
)

type fakeSeats struct {
	cells map[string]string
	urns  map[string]string
}

func (f fakeSeats) Cell(id string) string         { return f.cells[id] }
func (f fakeSeats) AgentForURN(urn string) string { return f.urns[urn] }

var photon = fakeSeats{
	cells: map[string]string{"photon-tether": "photon", "archie": "command"},
	urns: map[string]string{
		"msg://agent/agent-mux/agt_lpy0gym4eh": "photon-tether",
		"msg://agent/agent-mux/agt_lc7m97f5fp": "archie",
	},
}

func stampSeat(t *testing.T, s *Seat, e events.Event) events.Event {
	t.Helper()
	if err := s.Stamp(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestSeatStampsCellFromTheEventsAgent(t *testing.T) {
	s := NewSeat(photon)
	cases := []struct {
		name string
		e    events.Event
		want string
	}{
		{"logical agent", events.Event{Kind: "session.state_changed", Envelope: events.Envelope{Dims: events.ScopeDims{LogicalAgentID: "photon-tether"}}}, "photon"},
		{"legacy logical agent field", events.Event{Kind: "session.state_changed", LogicalAgentID: "archie"}, "command"},
		{"agent urn", events.Event{Kind: "tool.call.started", Envelope: events.Envelope{Dims: events.ScopeDims{AgentURN: "msg://agent/agent-mux/agt_lc7m97f5fp"}}}, "command"},
		{"mail read is the reader's", events.Event{Kind: "message.read", PayloadJSON: `{"from":"msg://agent/agent-mux/agt_lc7m97f5fp","to":["msg://agent/agent-mux/agt_lpy0gym4eh"]}`}, "photon"},
		{"mail sent is the sender's", events.Event{Kind: "message.sent", PayloadJSON: `{"from":"msg://agent/agent-mux/agt_lc7m97f5fp","to":["msg://agent/agent-mux/agt_lpy0gym4eh"]}`}, "command"},
		{"producer cell kept", events.Event{Kind: "x.y", LogicalAgentID: "archie", Envelope: events.Envelope{Dims: events.ScopeDims{Cell: "other"}}}, "other"},
		{"unknown agent", events.Event{Kind: "x.y", LogicalAgentID: "nobody"}, ""},
	}
	for _, tc := range cases {
		if got := stampSeat(t, s, tc.e).Envelope.Dims.Cell; got != tc.want {
			t.Errorf("%s: cell = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSeatSummaries(t *testing.T) {
	s := NewSeat(photon)
	cases := []struct {
		kind, payload, want string
	}{
		{"message.read", `{"from":"msg://agent/agent-mux/agt_lc7m97f5fp","to":["msg://agent/agent-mux/agt_lpy0gym4eh"],"msg_kind":"request"}`, "@photon-tether read request from @archie"},
		{"message.sent", `{"from":"msg://agent/agent-mux/agt_lc7m97f5fp","to":["msg://agent/agent-mux/someone"],"msg_kind":"notice"}`, "@archie sent notice to @someone"},
		{"tool.call.finished", `{"tool":"torque_task_get","server":"tether","ok":true,"duration_ms":42}`, "tool tether.torque_task_get finished in 42ms"},
		{"tool.call.finished", `{"tool":"x","ok":false,"error_class":"timeout"}`, "tool x failed (timeout)"},
		{"session.state_changed", `{"from":"starting","to":"running"}`, "session.state_changed starting → running"},
		{"session.compacted", `{"source":"compact","trigger":"auto","pre_tokens":150000}`, "context compacted (auto), 150000 tokens before"},
		{"session.cleared", `{"source":"clear"}`, "context cleared"},
		{"seat.waiting", `{"waiting_for":"approval"}`, "waiting for approval"},
		{"seat.unblocked", `{"waiting_for":"question","resolution":"resolved"}`, "unblocked (question)"},
		{"wake.outcome", `{"target":"msg://agent/agent-mux/agt_lpy0gym4eh","delivered":false,"reason":"busy"}`, "wake @photon-tether: not delivered (busy)"},
		{"session.output", `{"data":"c2VjcmV0","stream":"stdout"}`, "output on stdout"},
		{"daemon.started", ``, "daemon.started"},
		{"seat.status", `{"handle":"@photon-tether","state":"blocked","reason":"waiting on the harness tag","owner":"@archie"}`, "@photon-tether blocked: waiting on the harness tag (owner @archie)"},
		{"seat.status", `{"handle":"@photon-tether","state":"working"}`, "@photon-tether working"},
		{"mail.triaged", `{"handle":"@builder-2","from":"msg://agent/agent-mux/agt_x","class":"fyi","reason":"envelope_fyi"}`, "@builder-2 mail from @agt_x triaged fyi (envelope_fyi)"},
		{"seat.gate_blocked", `{"handle":"@builder-2","message_ids":["m1","m2"],"count":2}`, "@builder-2 stop gate blocked the turn on 2 pending message(s)"},
		{"message.sent", `{"from":"msg://agent/agent-mux/agt_lpy0gym4eh","to":["msg://agent/agent-mux/agt_lc7m97f5fp"],"msg_kind":"request","envelope_kind":"decision"}`, "@photon-tether sent decision to @archie"},
	}
	for _, tc := range cases {
		got := stampSeat(t, s, events.Event{Kind: tc.kind, PayloadJSON: tc.payload}).Envelope.Summary
		if got != tc.want {
			t.Errorf("%s: summary = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestSummaryIsOneRedactedBoundedLine(t *testing.T) {
	s := NewSeat(nil)
	e := stampSeat(t, s, events.Event{Kind: "x.y", Envelope: events.Envelope{Summary: "line one\nline two\ttoken ghp_" + strings.Repeat("a", 36) + " " + strings.Repeat("é", 300)}})
	got := e.Envelope.Summary
	if strings.ContainsAny(got, "\r\n\t") {
		t.Fatalf("summary not one line: %q", got)
	}
	if strings.Contains(got, "ghp_aaaa") {
		t.Fatalf("summary not redacted: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > events.SummaryMax {
		t.Fatalf("summary is %d runes, max %d", n, events.SummaryMax)
	}
	if !strings.HasPrefix(got, "line one line two") {
		t.Fatalf("producer summary not kept: %q", got)
	}
}

package messaging_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/messaging"
	"github.com/hollis-labs/tether/internal/registry"
)

// stubLookup is a deterministic Lookup for parser tests. URNs registers
// what Lookup returns; DisplayNames registers what FindByDisplayName
// returns for a given display_name short-form.
type stubLookup struct {
	URNs         map[string]registry.Profile
	DisplayNames map[string][]registry.Profile
	LookupErr    error
	FindErr      error
}

func (s *stubLookup) Lookup(_ context.Context, urn string) (registry.Profile, error) {
	if s.LookupErr != nil {
		return registry.Profile{}, s.LookupErr
	}
	p, ok := s.URNs[urn]
	if !ok {
		return registry.Profile{}, registry.ErrNotFound
	}
	return p, nil
}

func (s *stubLookup) FindByDisplayName(_ context.Context, name string) ([]registry.Profile, error) {
	if s.FindErr != nil {
		return nil, s.FindErr
	}
	return s.DisplayNames[name], nil
}

// recordingSender captures every Envelope sent for assertion.
type recordingSender struct {
	mu      sync.Mutex
	sent    []gomsg.Envelope
	sendErr error
}

func (r *recordingSender) Send(_ context.Context, env gomsg.Envelope) (gomsg.Envelope, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, env)
	return env, r.sendErr
}

// ─── Parse ───────────────────────────────────────────────────────────────────

func TestParser_Parse_FullURN(t *testing.T) {
	target := "msg://agent/agent-mux/agt_abcdefghij"
	lookup := &stubLookup{URNs: map[string]registry.Profile{
		target: {URN: target, DisplayName: "Alpha"},
	}}
	p := messaging.NewParser(lookup, &recordingSender{})

	payload := json.RawMessage(`{"text":"hello @msg://agent/agent-mux/agt_abcdefghij"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_xxxxxxxxxx")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d mentions; want 1", len(got))
	}
	if got[0].ResolvedURN != target {
		t.Errorf("ResolvedURN = %q; want %q", got[0].ResolvedURN, target)
	}
	if got[0].Source != "urn" {
		t.Errorf("Source = %q; want urn", got[0].Source)
	}
}

func TestParser_Parse_ShortForm(t *testing.T) {
	target := "msg://agent/agent-mux/agt_abcdefghij"
	lookup := &stubLookup{DisplayNames: map[string][]registry.Profile{
		"alpha": {{URN: target, DisplayName: "alpha"}},
	}}
	p := messaging.NewParser(lookup, &recordingSender{})

	payload := json.RawMessage(`{"text":"hi @alpha"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d mentions; want 1", len(got))
	}
	if got[0].ResolvedURN != target {
		t.Errorf("ResolvedURN = %q; want %q", got[0].ResolvedURN, target)
	}
	if got[0].Source != "display_name" {
		t.Errorf("Source = %q; want display_name", got[0].Source)
	}
}

func TestParser_Parse_AmbiguousShortForm(t *testing.T) {
	a := "msg://agent/agent-mux/agt_aaaaaaaaaa"
	b := "msg://agent/agent-mux/agt_bbbbbbbbbb"
	lookup := &stubLookup{DisplayNames: map[string][]registry.Profile{
		"alex": {{URN: a, DisplayName: "alex"}, {URN: b, DisplayName: "alex"}},
	}}
	p := messaging.NewParser(lookup, &recordingSender{})

	payload := json.RawMessage(`{"text":"@alex please review"}`)
	_, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	var amb *registry.ErrAmbiguousMention
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v; want *ErrAmbiguousMention", err)
	}
	if amb.Token != "alex" {
		t.Errorf("Token = %q; want alex", amb.Token)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("Candidates = %v; want 2", amb.Candidates)
	}
}

func TestParser_Parse_EscapedAt(t *testing.T) {
	lookup := &stubLookup{}
	p := messaging.NewParser(lookup, &recordingSender{})
	payload := json.RawMessage(`{"text":"use \\@token to send a literal"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d mentions; want 0 (escaped)", len(got))
	}
}

func TestParser_Parse_BotMentionSilent(t *testing.T) {
	grpURN := "msg://group/agent-mux/grp_xxxxxxxxxx"
	lookup := &stubLookup{}
	p := messaging.NewParser(lookup, &recordingSender{})
	payload := json.RawMessage(`{"text":"FYI @msg://group/agent-mux/grp_xxxxxxxxxx"}`)
	got, err := p.Parse(context.Background(), payload, grpURN)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d mentions; want 0 (bot-mention)", len(got))
	}
}

func TestParser_Parse_DedupesDuplicates(t *testing.T) {
	target := "msg://agent/agent-mux/agt_dededededed"
	target = target[:len(target)-1] // trim to 10-alnum
	lookup := &stubLookup{DisplayNames: map[string][]registry.Profile{
		"alpha": {{URN: target, DisplayName: "alpha"}},
	}}
	p := messaging.NewParser(lookup, &recordingSender{})
	payload := json.RawMessage(`{"text":"@alpha @alpha @alpha"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d mentions; want 1 (deduped)", len(got))
	}
}

func TestParser_Parse_UnknownURNKept(t *testing.T) {
	lookup := &stubLookup{} // empty — Lookup returns ErrNotFound
	p := messaging.NewParser(lookup, &recordingSender{})
	payload := json.RawMessage(`{"text":"@msg://agent/agent-mux/agt_unknownnnn"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d mentions; want 1 (unresolved kept)", len(got))
	}
	if got[0].ResolvedURN != "" {
		t.Errorf("ResolvedURN = %q; want empty (unresolved)", got[0].ResolvedURN)
	}
}

func TestParser_Parse_UnknownShortFormDropped(t *testing.T) {
	lookup := &stubLookup{DisplayNames: map[string][]registry.Profile{}}
	p := messaging.NewParser(lookup, &recordingSender{})
	payload := json.RawMessage(`{"text":"random @notfound text"}`)
	got, err := p.Parse(context.Background(), payload, "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d mentions; want 0 (unknown short-form is text)", len(got))
	}
}

func TestParser_Parse_RespectsMaxMentions(t *testing.T) {
	const total = messaging.MaxMentions + 5
	lookup := &stubLookup{URNs: map[string]registry.Profile{}}
	// Generate `total` unique URNs; only MaxMentions should land.
	// Vary the last two suffix chars across base36 to guarantee uniqueness.
	const alpha = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for i := 0; i < total; i++ {
		hi := alpha[i/len(alpha)]
		lo := alpha[i%len(alpha)]
		urn := "msg://agent/agent-mux/agt_aaaaaaaa" + string(hi) + string(lo)
		lookup.URNs[urn] = registry.Profile{URN: urn}
		b.WriteString(" @")
		b.WriteString(urn)
	}
	p := messaging.NewParser(lookup, &recordingSender{})
	got, err := p.Parse(context.Background(), json.RawMessage(`{"text":"`+b.String()+`"}`), "msg://group/agent-mux/grp_g")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != messaging.MaxMentions {
		t.Errorf("got %d mentions; want %d (cap)", len(got), messaging.MaxMentions)
	}
}

// ─── Dispatch ────────────────────────────────────────────────────────────────

func TestParser_Dispatch_EmitsOneNoticePerResolvedURN(t *testing.T) {
	target1 := "msg://agent/agent-mux/agt_aaaaaaaaaa"
	target2 := "msg://agent/agent-mux/agt_bbbbbbbbbb"
	sender := &recordingSender{}
	p := messaging.NewParser(&stubLookup{}, sender)
	gm := registry.GroupMessage{
		ID: "01HXTEST", GroupURN: "msg://group/agent-mux/grp_g",
		GroupSeq: 7, FromURN: "msg://agent/agent-mux/agt_sender0000",
		ThreadID: "t1", Payload: json.RawMessage(`{"text":"important"}`),
		CreatedAt: time.Now(),
	}
	p.Dispatch(context.Background(), gm, []registry.Mention{
		{Token: "alpha", ResolvedURN: target1, Source: "display_name"},
		{Token: "beta", ResolvedURN: target2, Source: "display_name"},
	})
	if got := len(sender.sent); got != 2 {
		t.Fatalf("got %d sends; want 2", got)
	}
	// Spot-check payload shape.
	for i, env := range sender.sent {
		if env.Kind != "notice" {
			t.Errorf("sent[%d].Kind = %q; want notice", i, env.Kind)
		}
		var payload map[string]any
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			t.Fatalf("sent[%d] payload unmarshal: %v", i, err)
		}
		if payload["group"] != "msg://group/agent-mux/grp_g" {
			t.Errorf("sent[%d].payload.group = %v; want grp_g", i, payload["group"])
		}
		if payload["message_id"] != "01HXTEST" {
			t.Errorf("sent[%d].payload.message_id = %v; want 01HXTEST", i, payload["message_id"])
		}
	}
}

func TestParser_Dispatch_SkipsUnresolved(t *testing.T) {
	sender := &recordingSender{}
	p := messaging.NewParser(&stubLookup{}, sender)
	gm := registry.GroupMessage{ID: "01H", GroupURN: "msg://group/agent-mux/grp_g"}
	p.Dispatch(context.Background(), gm, []registry.Mention{
		{Token: "ghost", ResolvedURN: ""}, // unresolved
	})
	if got := len(sender.sent); got != 0 {
		t.Errorf("got %d sends; want 0 (unresolved skipped)", got)
	}
}

func TestParser_Dispatch_LogsSendErrorButContinues(t *testing.T) {
	target1 := "msg://agent/agent-mux/agt_aaaaaaaaaa"
	target2 := "msg://agent/agent-mux/agt_bbbbbbbbbb"
	sender := &recordingSender{sendErr: errors.New("transport failure")}
	p := messaging.NewParser(&stubLookup{}, sender)
	gm := registry.GroupMessage{ID: "01H", GroupURN: "msg://group/agent-mux/grp_g"}
	// Even though Send errors on the first call, Dispatch should keep
	// going — fire-and-forget per D11/D12.
	p.Dispatch(context.Background(), gm, []registry.Mention{
		{Token: "a", ResolvedURN: target1},
		{Token: "b", ResolvedURN: target2},
	})
	if got := len(sender.sent); got != 2 {
		t.Errorf("got %d sends; want 2 (Dispatch continued through error)", got)
	}
}

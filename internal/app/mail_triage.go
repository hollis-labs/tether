package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/messaging/triage"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/store"
)

// Hub mail triage before wake (CW-20261114-0017, design v2 section 5). For a
// seat with mail_triage on, each message is classified once (triage.Classify)
// and the wake paths ask gate before waking:
//
//   - human  wakes now, never capped;
//   - action wakes at most once per seat per action_interval; a later one is
//     deferred to the end of the interval, and a wake marks the seat's other
//     pending action mail covered (the wake text counts all unread mail);
//   - fyi    never wakes; it rides along as one line on the seat's next
//     human-originated turn (and typed fyi is then marked read);
//   - read   never wakes; a typed envelope with attention=none is marked read
//     by the hub at once (message.read).
//
// A seat without mail_triage (the default) is woken exactly as before.

// KindMailTriaged is published once per triaged message.
const KindMailTriaged = "mail.triaged"

// seatPolicy says how mail to an address is triaged: the seat it belongs to,
// its @handle, whether triage is on and the action-wake interval.
type seatPolicy struct {
	seat     string
	handle   string
	enabled  bool
	interval time.Duration
}

type mailTriage struct {
	st      *store.Store
	policy  func(to messaging.Address) seatPolicy
	publish func(context.Context, events.Event) error
	now     func() time.Time
	// handle names an address (@handle) for summaries and the fyi line; nil
	// uses its last path segment.
	handle func(ctx context.Context, urn string) string
}

func (m *mailTriage) nameOf(ctx context.Context, urn string) string {
	if m.handle != nil {
		if h := m.handle(ctx, urn); h != "" {
			return h
		}
	}
	return handleOf(urn)
}

type triageDecision struct {
	wake   bool
	class  triage.Class
	reason string    // wake-outcome reason when not waking
	until  time.Time // deferred-cap: earliest wake
}

func (m *mailTriage) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now().UTC()
}

// gate decides whether env may wake its recipient now. A nil triage, a seat
// without triage, or any storage error wakes as before (fail open: triage
// must never lose mail).
func (m *mailTriage) gate(ctx context.Context, env messaging.Envelope) triageDecision {
	if m == nil || m.st == nil || m.policy == nil {
		return triageDecision{wake: true}
	}
	pol := m.policy(env.To)
	if !pol.enabled {
		return triageDecision{wake: true}
	}
	now := m.clock()
	row, err := m.record(ctx, env, pol, now)
	if err != nil {
		log.Printf("app: mail triage for %s: %v (waking as before)", env.ID, err)
		return triageDecision{wake: true}
	}
	class := triage.Class(row.Class)
	switch row.State {
	case store.TriageCovered:
		return triageDecision{class: class, reason: "triaged-covered"}
	case store.TriageSurfaced, store.TriageSettled:
		return triageDecision{class: class, reason: "triaged-" + string(class)}
	}
	switch class {
	case triage.Human:
		return triageDecision{wake: true, class: class}
	case triage.FYI, triage.Read:
		return triageDecision{class: class, reason: "triaged-" + string(class)}
	case triage.Action:
		// capped below
	}
	last, err := m.st.SeatLastActionWake(ctx, pol.seat)
	if err != nil {
		return triageDecision{wake: true, class: class}
	}
	if !last.IsZero() && pol.interval > 0 && now.Before(last.Add(pol.interval)) {
		until := last.Add(pol.interval)
		if row.NotBefore.IsZero() || !row.NotBefore.Equal(until) {
			_ = m.st.TriageDefer(ctx, env.ID, until, now)
		}
		return triageDecision{class: class, reason: "deferred-cap", until: until}
	}
	return triageDecision{wake: true, class: class}
}

// record classifies env the first time it is seen, publishes mail.triaged and
// settles auto-settled mail; later calls return the stored triage.
func (m *mailTriage) record(ctx context.Context, env messaging.Envelope, pol seatPolicy, now time.Time) (store.TriageRow, error) {
	if row, found, err := m.st.TriageGet(ctx, env.ID); err != nil || found {
		return row, err
	}
	urgency := env.Metadata["urgency"]
	if intent, err := wakeintent.Read(env); err == nil && intent.Urgency != "" {
		urgency = intent.Urgency
	}
	res := triage.Classify(triage.Input{From: env.From.URN(), Kind: env.Kind, ContentType: env.ContentType, Payload: env.Payload, Urgency: urgency, RecipientHandle: pol.handle})
	row, created, err := m.st.TriageRecord(ctx, store.TriageRow{MessageID: env.ID, Recipient: env.To.URN(), Seat: pol.seat, Class: string(res.Class), Reason: res.Reason, Typed: res.Typed, CreatedAt: now})
	if err != nil || !created {
		return row, err
	}
	if res.AutoSettle {
		if err := m.st.MessagingStore().MarkRead(ctx, env.ID, env.To); err != nil {
			log.Printf("app: mail triage: settle %s: %v", env.ID, err)
		} else if err := m.st.TriageSetState(ctx, env.ID, store.TriageSettled, now); err == nil {
			row.State = store.TriageSettled
		}
	}
	m.emit(ctx, env, pol, res)
	return row, nil
}

func (m *mailTriage) emit(ctx context.Context, env messaging.Envelope, pol seatPolicy, res triage.Result) {
	if m.publish == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"message_id": env.ID, "recipient": env.To.URN(), "seat": pol.seat, "handle": pol.handle,
		"class": string(res.Class), "reason": res.Reason, "typed": res.Typed, "from": env.From.URN(), "from_handle": m.nameOf(ctx, env.From.URN())})
	e := events.Event{Scope: events.ScopeDaemon, Kind: KindMailTriaged, At: m.clock(), PayloadJSON: string(raw)}
	e.Envelope.Dims.AgentURN = pol.seat
	e.Envelope.Class.Category = "message"
	e.Envelope.Corr.CorrelationID = env.ID
	if err := m.publish(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("app: mail triage: publish %s: %v", KindMailTriaged, err)
	}
}

// woke records a delivered wake of env's seat: the message is woken, an action
// wake starts the seat's interval, and the seat's other pending action mail is
// covered by this wake.
func (m *mailTriage) woke(ctx context.Context, env messaging.Envelope) {
	if m == nil || m.st == nil || m.policy == nil {
		return
	}
	pol := m.policy(env.To)
	if !pol.enabled {
		return
	}
	now := m.clock()
	row, found, err := m.st.TriageGet(ctx, env.ID)
	if err != nil || !found {
		return
	}
	_ = m.st.TriageSetState(ctx, env.ID, store.TriageWoken, now)
	if row.Class == string(triage.Action) {
		_ = m.st.SetSeatLastActionWake(ctx, pol.seat, now)
	}
	_, _ = m.st.TriageCoverPending(ctx, pol.seat, env.ID, now)
}

// fyiLine is the one line a human-originated turn to the seat carries about its
// pending fyi mail ("" when there is none), and the commit to run once that
// turn was accepted: it marks the listed mail surfaced and typed fyi read
// (untyped stays unread for the inbox).
func (m *mailTriage) fyiLine(ctx context.Context, pol seatPolicy) (string, func()) {
	if m == nil || m.st == nil || !pol.enabled {
		return "", func() {}
	}
	items, err := m.st.TriageFYIPending(ctx, pol.seat, 20)
	if err != nil || len(items) == 0 {
		return "", func() {}
	}
	parts := make([]string, 0, 5)
	for i, it := range items {
		if i == 5 {
			break
		}
		label := m.nameOf(ctx, it.From)
		if it.Subject != "" {
			label += ": " + it.Subject
		}
		parts = append(parts, label)
	}
	more := ""
	if len(items) > 5 {
		more = fmt.Sprintf("; and %d more", len(items)-5)
	}
	line := fmt.Sprintf("FYI: %d unread mail(s): %s%s (tether_message_list to read them).", len(items), strings.Join(parts, "; "), more)
	commit := func() {
		now := m.clock()
		for _, it := range items {
			_ = m.st.TriageSetState(context.WithoutCancel(ctx), it.MessageID, store.TriageSurfaced, now)
			if it.Typed {
				if to, err := messaging.ParseURN(it.Recipient); err == nil {
					_ = m.st.MessagingStore().MarkRead(context.WithoutCancel(ctx), it.MessageID, to)
				}
			}
		}
	}
	return line, commit
}

// handleOf is a short name for an address: its last path segment.
func handleOf(urn string) string {
	if i := strings.LastIndexByte(urn, '/'); i >= 0 && i < len(urn)-1 {
		return "@" + urn[i+1:]
	}
	return urn
}

// newMailTriage builds the triage over the live catalog.
func (s *Service) newMailTriage() *mailTriage {
	if s.Store == nil {
		return nil
	}
	m := &mailTriage{st: s.Store, policy: s.seatPolicyFor, handle: s.senderHandle}
	if s.Bus != nil {
		m.publish = s.Bus.Publish
	}
	return m
}

// seatPolicyFor maps a recipient address to its catalog seat and policy. Mail
// to an address that is no catalog agent is not triaged.
func (s *Service) seatPolicyFor(to messaging.Address) seatPolicy {
	cat := s.CurrentCatalog()
	if cat == nil || to.Kind != messaging.KindAgent {
		return seatPolicy{}
	}
	urn := to.URN()
	for id, a := range cat.Agents {
		if urn != a.URN && urn != "msg://agent/agent-mux/"+id {
			continue
		}
		enabled, interval := config.EffectiveMailTriage(cat.Global, a)
		seat := a.URN
		if seat == "" {
			seat = "msg://agent/agent-mux/" + id
		}
		handle := a.Handle
		if handle == "" {
			handle = id
		}
		return seatPolicy{seat: seat, handle: "@" + handle, enabled: enabled, interval: interval}
	}
	return seatPolicy{}
}

// seatPolicyForSession is the policy of the seat a session runs.
func (s *Service) seatPolicyForSession(sessionID string) seatPolicy {
	if s.Store == nil {
		return seatPolicy{}
	}
	row, err := s.Store.GetSession(sessionID)
	if err != nil || row == nil || row.LogicalAgentID == "" {
		return seatPolicy{}
	}
	return s.seatPolicyFor(messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: row.LogicalAgentID})
}

// senderHandle names an address as @handle: the operator is @chrispian; a
// catalog agent (by urn: or agent-mux form) its handle: or id; otherwise the
// registry's handle, then its tether id. "" when none applies.
func (s *Service) senderHandle(ctx context.Context, urn string) string {
	if urn == triage.OperatorURN {
		return "@chrispian"
	}
	if cat := s.CurrentCatalog(); cat != nil {
		for id, a := range cat.Agents {
			if urn == a.URN || urn == "msg://agent/agent-mux/"+id {
				if a.Handle != "" {
					return "@" + a.Handle
				}
				return "@" + id
			}
		}
	}
	if s.Registry != nil {
		if ids, err := s.Registry.LookupExternalIDsForURN(ctx, urn); err == nil {
			for _, substrate := range []string{"handle", "tether"} {
				for _, id := range ids {
					if id.Substrate == substrate && id.ExternalID != "" {
						return "@" + id.ExternalID
					}
				}
			}
		}
	}
	return ""
}

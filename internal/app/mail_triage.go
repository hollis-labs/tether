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
	"github.com/hollis-labs/tether/internal/messaging/envelope"
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
//     deferred to the end of the interval (a debounce: nothing is dropped). A
//     reply to mail the seat sent is action whatever its kind (reply_to_own,
//     CW-20261117-0002);
//   - fyi    never wakes on its own; it rides along on the seat's next wake
//     (listed in the coalesced digest) or human-originated turn (one line),
//     and typed fyi is then marked read;
//
// Every wake of a triaged seat is coalesced (CW-20261117-0002): its text lists
// the seat's other pending action and fyi mail, and once the wake is delivered
// those rows are marked woken / surfaced, so one turn carries everything and
// no later wake repeats it.
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
	// forms are the addresses the seat sends and receives as (its urn and its
	// agent-mux catalog-id form), for reply_to_own.
	forms []string
	// agentID is the seat's catalog id (its logical agent), for resolving its
	// live session.
	agentID string
}

// owns reports whether urn is one of the seat's own addresses.
func (p seatPolicy) owns(urn string) bool {
	if urn == "" {
		return false
	}
	if urn == p.seat {
		return true
	}
	for _, f := range p.forms {
		if urn == f {
			return true
		}
	}
	return false
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
		// attention:none never wakes a seat, triaged or not (CW-20261118-0050):
		// it stays unread and visible, and the next real wake's unread count
		// carries it. Triaged seats get the same through the Read class below.
		if m.quietNone(ctx, env, pol) {
			return triageDecision{class: triage.Read, reason: "attention-none"}
		}
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

// quietNone reports whether env is an explicit attention=none typed envelope
// that no action rule overrides (operator, urgency high, reply to the seat's own
// mail, on_behalf_of): the same answer triage.Classify gives, for a seat that
// has no triage record.
func (m *mailTriage) quietNone(ctx context.Context, env messaging.Envelope, pol seatPolicy) bool {
	if env.ContentType != envelope.ContentType {
		return false
	}
	urgency := env.Metadata["urgency"]
	if intent, err := wakeintent.Read(env); err == nil && intent.Urgency != "" {
		urgency = intent.Urgency
	}
	res := triage.Classify(triage.Input{From: m.operatorForm(ctx, env.From.URN()), Kind: env.Kind, ContentType: env.ContentType, Payload: env.Payload, Urgency: urgency, RecipientHandle: pol.handle, ReplyToOwn: m.replyToOwn(ctx, env, pol)})
	return res.Reason == "attention_none"
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
	res := triage.Classify(triage.Input{From: m.operatorForm(ctx, env.From.URN()), Kind: env.Kind, ContentType: env.ContentType, Payload: env.Payload, Urgency: urgency, RecipientHandle: pol.handle, ReplyToOwn: m.replyToOwn(ctx, env, pol)})
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

// replyToOwn reports whether env answers mail its recipient seat sent: its
// in_reply_to names a message whose sender is one of the seat's addresses.
func (m *mailTriage) replyToOwn(ctx context.Context, env messaging.Envelope, pol seatPolicy) bool {
	if env.InReplyTo == "" {
		return false
	}
	parent, err := m.st.MessagingStore().Get(ctx, env.InReplyTo)
	if err != nil {
		return false
	}
	from := parent.From.URN()
	return from == env.To.URN() || pol.owns(from)
}

func (m *mailTriage) emit(ctx context.Context, env messaging.Envelope, pol seatPolicy, res triage.Result) {
	if m.publish == nil {
		return
	}
	payload := map[string]any{"message_id": env.ID, "recipient": env.To.URN(), "seat": pol.seat, "handle": pol.handle,
		"class": string(res.Class), "reason": res.Reason, "typed": res.Typed, "from": env.From.URN(), "from_handle": m.nameOf(ctx, env.From.URN())}
	// Mail an assistant sent for someone says so wherever it is triaged (CW-20261114-0116).
	if behalf, drafted := envelope.Behalf(env.ContentType, env.Payload); behalf != "" {
		payload["on_behalf_of"], payload["drafted_by"] = behalf, string(drafted)
	}
	raw, _ := json.Marshal(payload)
	e := events.Event{Scope: events.ScopeDaemon, Kind: KindMailTriaged, At: m.clock(), PayloadJSON: string(raw)}
	e.Envelope.Dims.AgentURN = pol.seat
	e.Envelope.Class.Category = "message"
	e.Envelope.Corr.CorrelationID = env.ID
	if err := m.publish(context.WithoutCancel(ctx), e); err != nil {
		log.Printf("app: mail triage: publish %s: %v", KindMailTriaged, err)
	}
}

// woke records a delivered wake of env's seat: the message is woken and an
// action wake starts the seat's interval. The seat's other pending mail is not
// touched here: the digest the wake carried settles what it listed
// (CW-20261117-0002 removed the blanket "covered" that left mail unread and
// unnamed).
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
}

// digestLimit caps the messages a coalesced wake names one by one.
const digestLimit = 15

// digest is the coalesced part of a wake to env's seat (CW-20261117-0002): the
// seat's other pending action and fyi mail, one line each, and the commit to
// run once the wake was delivered, which marks every pending row the digest
// counted covered (action: named by this wake, so it never wakes on its own)
// or surfaced (fyi, typed fyi also read). "" and a
// no-op when the seat is not triaged or has nothing else pending.
func (m *mailTriage) digest(ctx context.Context, env messaging.Envelope) (string, func()) {
	noop := func() {}
	if m == nil || m.st == nil || m.policy == nil {
		return "", noop
	}
	pol := m.policy(env.To)
	if !pol.enabled {
		return "", noop
	}
	items, _, err := m.st.TriageSeatPending(ctx, pol.seat, 500)
	if err != nil {
		return "", noop
	}
	others := items[:0:0]
	for _, it := range items {
		if it.MessageID != env.ID {
			others = append(others, it)
		}
	}
	if len(others) == 0 {
		return "", noop
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Also pending for you (%d):", len(others))
	for i, it := range others {
		if i == digestLimit {
			fmt.Fprintf(&b, "\n- and %d more (tether_message_inbox to list them)", len(others)-digestLimit)
			break
		}
		label := m.nameOf(ctx, it.From)
		if it.Subject != "" {
			label += ": " + it.Subject
		}
		tag := ""
		switch {
		case it.Reason == "reply_to_own":
			tag = " [reply to you]"
		case it.Class == string(triage.FYI):
			tag = " [fyi]"
		}
		fmt.Fprintf(&b, "\n- `%s` %s%s", it.MessageID, label, tag)
	}
	commit := func() {
		now := m.clock()
		cctx := context.WithoutCancel(ctx)
		for _, it := range others {
			// Covered: this wake named it, so it never wakes on its own.
			state := store.TriageCovered
			if it.Class == string(triage.FYI) {
				state = store.TriageSurfaced
			}
			_ = m.st.TriageSetState(cctx, it.MessageID, state, now)
			if it.Class == string(triage.FYI) && it.Typed {
				if to, err := messaging.ParseURN(it.Recipient); err == nil {
					_ = m.st.MessagingStore().MarkRead(cctx, it.MessageID, to)
				}
			}
		}
	}
	return b.String(), commit
}

// withDigest appends env's coalesced digest to a wake text. The returned
// commit must run only once the wake was delivered.
func (m *mailTriage) withDigest(ctx context.Context, env messaging.Envelope, text string) (string, func()) {
	line, commit := m.digest(ctx, env)
	if line == "" {
		return text, commit
	}
	return text + "\n\n" + line, commit
}

// enabledFor reports whether mail to to is triaged.
func (m *mailTriage) enabledFor(to messaging.Address) bool {
	return m != nil && m.policy != nil && m.policy(to).enabled
}

// backfillReplyToOwn re-classifies pending fyi mail that answers mail its seat
// sent as action/reply_to_own (CW-20261117-0002): before the rule existed such
// replies were filed as fyi and never woke anyone. The wake sweep then wakes
// each seat once, coalesced. Idempotent: only pending fyi rows move. It returns
// the candidates examined, the rows moved and the seats they belong to.
func (m *mailTriage) backfillReplyToOwn(ctx context.Context) (examined, moved int, seats map[string]int, err error) {
	seats = map[string]int{}
	if m == nil || m.st == nil || m.policy == nil {
		return 0, 0, seats, nil
	}
	cands, err := m.st.TriageFYIReplies(ctx)
	if err != nil {
		return 0, 0, seats, err
	}
	now := m.clock()
	for _, c := range cands {
		examined++
		owned := c.ParentFrom == c.Recipient || c.ParentFrom == c.Seat
		if !owned {
			if to, perr := messaging.ParseURN(c.Recipient); perr == nil {
				owned = m.policy(to).owns(c.ParentFrom)
			}
		}
		if !owned {
			continue
		}
		ok, uerr := m.st.TriageReclassify(ctx, c.MessageID, string(triage.Action), "reply_to_own", now)
		if uerr != nil {
			return examined, moved, seats, uerr
		}
		if ok {
			moved++
			seats[c.Seat]++
		}
	}
	return examined, moved, seats, nil
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
		forms := []string{"msg://agent/agent-mux/" + id}
		if a.URN != "" {
			forms = append(forms, a.URN)
		}
		return seatPolicy{seat: seat, handle: "@" + handle, enabled: enabled, interval: interval, forms: forms, agentID: id}
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
	if operatorForm(ctx, s.Store, urn) == triage.OperatorURN {
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
					// Registry ids are caller-registered: only a valid handle,
					// never the operator's, may name a sender (a non-catalog
					// sender must not render as @chrispian).
					if id.Substrate == substrate && envelope.ValidHandle("@"+id.ExternalID) && id.ExternalID != "chrispian" {
						return "@" + id.ExternalID
					}
				}
			}
		}
	}
	return ""
}

// operatorForm returns triage.OperatorURN for a user form of the operator's
// mailbox (his canonical address or another user alias of it, CW-20261114-0067)
// and urn otherwise, so mail the operator sends under his canonical address is
// still the operator's. An agent address aliased into his mailbox is never him
// (@archie's ruling): mail from it triages as the agent's. A failed alias read
// leaves urn as it is.
func operatorForm(ctx context.Context, st *store.Store, urn string) string {
	if urn == triage.OperatorURN || st == nil || !strings.HasPrefix(urn, "msg://user/") {
		return urn
	}
	forms, err := st.AddressForms(ctx, urn)
	if err != nil {
		return urn
	}
	for _, f := range forms {
		if f == triage.OperatorURN {
			return triage.OperatorURN
		}
	}
	return urn
}

func (m *mailTriage) operatorForm(ctx context.Context, urn string) string {
	return operatorForm(ctx, m.st, urn)
}

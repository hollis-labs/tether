package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func addr(t *testing.T, urn string) messaging.Address {
	t.Helper()
	a, err := messaging.ParseURN(urn)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The author of a stored message is the principal the daemon verified, never the From
// it claims: the shared operator credential may claim any From, so it proves nothing.
func TestClassifyStampedSender(t *testing.T) {
	meta := func(state, principal string) map[string]string {
		m := map[string]string{api.MetaFromVerification: state}
		if principal != "" {
			m[api.MetaFromPrincipal] = principal
		}
		return m
	}
	const op = "msg://user/local/operator"
	for name, tc := range map[string]struct {
		from  string
		meta  map[string]string
		class string
		basis string
	}{
		// Tachyon's own case: an agent From sent through the shared operator credential.
		"agent From via the shared operator credential, verified": {"msg://agent/agent-mux/agt_bl3jn0l3oq", meta("verified", op), store.ClassUnknown, store.BasisSharedOperator},
		"user From, stamped by the shared operator credential":    {op, meta("stamped", op), store.ClassUnknown, store.BasisSharedOperator},
		"user From claimed by the shared operator credential":     {"msg://user/local/alice", meta("verified", op), store.ClassUnknown, store.BasisSharedOperator},
		"session principal, its own address":                      {"msg://session/local/s9", meta("stamped", "msg://session/local/s9"), store.ClassAgent, store.BasisStampedSender},
		"agent principal":                                         {"msg://agent/agent-mux/agt_x", meta("verified", "msg://agent/agent-mux/agt_x"), store.ClassAgent, store.BasisStampedSender},
		"service principal":                                       {"msg://service/cerberus/dev", meta("verified", "msg://service/cerberus/dev"), store.ClassSystem, store.BasisStampedSender},
		"device principal (scopes unknown here)":                  {"msg://user/local/alice", meta("verified", "msg://device/tachyon"), store.ClassUnknown, store.BasisNone},
		"unverified stamp":                                        {"msg://agent/agent-mux/agt_x", meta("unverified", "msg://agent/agent-mux/agt_x"), store.ClassUnknown, store.BasisNone},
		"no stamp at all":                                         {"msg://agent/agent-mux/agt_x", nil, store.ClassUnknown, store.BasisNone},
		"verified but no recorded principal":                      {"msg://agent/agent-mux/agt_x", meta("verified", ""), store.ClassUnknown, store.BasisNone},
		"unparseable principal":                                   {"msg://agent/agent-mux/agt_x", meta("verified", "garbage"), store.ClassUnknown, store.BasisNone},
	} {
		t.Run(name, func(t *testing.T) {
			_, class, basis := classifyStampedSender(addr(t, tc.from), tc.meta)
			if class != tc.class || basis != tc.basis {
				t.Fatalf("class %q basis %q, want %q %q", class, basis, tc.class, tc.basis)
			}
		})
	}
	// Negative sweep: no combination of From kind, stamp state and principal yields
	// operator. Operator audience comes from authenticated routes only.
	for _, kind := range []string{"user", "agent", "session", "service", "group"} {
		for _, state := range []string{"", "stamped", "verified", "unverified"} {
			for _, principal := range []string{"", op, "msg://session/local/x", "msg://agent/a/b", "msg://service/s/e", "msg://device/d"} {
				if _, class, _ := classifyStampedSender(addr(t, "msg://"+kind+"/local/x"), meta(state, principal)); class == store.ClassOperator {
					t.Fatalf("From kind %s stamp %q principal %q classified operator", kind, state, principal)
				}
			}
		}
	}
}

func workerOutputService(t *testing.T) *Service {
	t.Helper()
	s, _ := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final", "failure"}})
	if _, err := s.Store.DB().Exec(`UPDATE sessions SET worker_id='worker',workspace=?,state='running' WHERE id='s1'`, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.Workers = workerhub.New(s.Store.DB())
	return s
}

func resultLine(turn, text string) []byte {
	b, _ := json.Marshal(map[string]any{"type": "result", "uuid": turn, "result": text})
	return append(b, '\n')
}

func deviceCtx(scopes ...string) context.Context {
	return identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://device/tachyon", Kind: "device", Verified: true, Scopes: scopes})
}

// End to end for a worker-hosted session: the input is recorded with its class
// before the text is sent, the result frame binds it, and the published event, the
// staged routed message and the retry journal all carry the audience, without the
// sender URN.
func TestRoutedOutputCarriesTheAudienceOfItsTriggeringInput(t *testing.T) {
	s := workerOutputService(t)
	done := s.beginTurnInput(deviceCtx("maintain", "operate", "read"), "s1")
	done(nil) // the worker acknowledged the input
	frame := workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-1", "hello operator")}
	if err := s.ingestWorkerFrame("worker", frame); err != nil {
		t.Fatal(err)
	}
	outs := outputEvents(t, s)
	if len(outs) != 1 {
		t.Fatalf("outputs: %+v", outs)
	}
	o := outs[0]
	if o.Audience != "operator" || o.AudienceBasis != store.BasisPrincipal || len(o.InputClasses) != 1 || o.InputClasses[0] != "operator" || len(o.InputIDs) != 1 {
		t.Fatalf("event audience: %+v", o)
	}
	staged, err := s.Store.StagedTurnOutput(context.Background(), o.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	m := staged.Metadata
	if m["audience"] != "operator" || m["audience_basis"] != store.BasisPrincipal || m["input_classes"] != "operator" || m["input_ids"] != o.InputIDs[0] {
		t.Fatalf("staged metadata: %v", m)
	}
	for k, v := range m {
		if strings.Contains(v, "msg://device") || strings.Contains(k, "sender") {
			t.Fatalf("the sender identity leaked onto the output: %s=%s", k, v)
		}
	}
	raw, _ := json.Marshal(o)
	if strings.Contains(string(raw), "msg://device") {
		t.Fatalf("sender URN in the event payload: %s", raw)
	}
	// The URN lives only in turn_inputs.
	var urn string
	if err := s.Store.DB().QueryRow(`SELECT sender_urn FROM turn_inputs WHERE input_id=?`, o.InputIDs[0]).Scan(&urn); err != nil || urn != "msg://device/tachyon" {
		t.Fatalf("sender_urn %q (%v)", urn, err)
	}
}

// An unverified or anonymous caller can never produce audience=operator, whatever
// scopes it claims; a worker-only device is system.
func TestUnverifiedCallerNeverProducesOperatorAudience(t *testing.T) {
	for name, tc := range map[string]struct {
		ctx      context.Context
		audience string
	}{
		"anonymous":                  {context.Background(), "unknown"},
		"unverified device, operate": {identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://device/x", Kind: "device", Verified: false, Scopes: []string{"operate"}}), "unknown"},
		"worker-only device":         {deviceCtx("worker"), "system"},
		"session principal":          {identity.WithPrincipal(context.Background(), identity.Principal{ID: "msg://session/local/s9", Kind: "session", Verified: true}), "agent"},
	} {
		t.Run(name, func(t *testing.T) {
			s := workerOutputService(t)
			s.beginTurnInput(tc.ctx, "s1")(nil)
			if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-1", "x")}); err != nil {
				t.Fatal(err)
			}
			outs := outputEvents(t, s)
			if len(outs) != 1 || outs[0].Audience != tc.audience {
				t.Fatalf("audience %+v, want %s", outs, tc.audience)
			}
			if outs[0].Audience == "operator" {
				t.Fatal("an unverified caller produced operator")
			}
		})
	}
}

// History and unrecorded sessions: no audience at all, which reads as unknown.
func TestOutputWithNoRecordedInputIsUnknown(t *testing.T) {
	s := workerOutputService(t)
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-1", "unprompted")}); err != nil {
		t.Fatal(err)
	}
	outs := outputEvents(t, s)
	if len(outs) != 1 || outs[0].Audience != "unknown" || outs[0].AudienceBasis != "no_input" || len(outs[0].InputClasses) != 0 {
		t.Fatalf("%+v", outs)
	}
}

// The crash/failure case at the SendTurn funnel: a send the worker could not take
// is recorded as failed and never binds to a later, unrelated output.
func TestFailedSendNeverBindsToALaterTurn(t *testing.T) {
	s := workerOutputService(t)
	err := s.SendTurn(deviceCtx("operate"), "s1", "this will not reach a worker")
	if err == nil {
		t.Fatal("the offline worker accepted a turn")
	}
	var state string
	if err := s.Store.DB().QueryRow(`SELECT state FROM turn_inputs WHERE session_id='s1'`).Scan(&state); err != nil || state != store.InputFailed {
		t.Fatalf("input state %q (%v), want failed", state, err)
	}
	// A provider-initiated output arrives later: it must not claim the failed input.
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-9", "autonomous")}); err != nil {
		t.Fatal(err)
	}
	outs := outputEvents(t, s)
	if len(outs) != 1 || outs[0].Audience != "unknown" || len(outs[0].InputIDs) != 0 {
		t.Fatalf("a failed input bound to a later turn: %+v", outs)
	}
}

// Replay: the journal carries the audience, so a republished output is identical.
func TestJournalReplayKeepsTheAudience(t *testing.T) {
	s := workerOutputService(t)
	row, err := s.Store.GetSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	job := &turnOutputWrite{row: *row, audience: &store.TurnAudience{Audience: "operator", Basis: store.BasisPrincipal, InputIDs: []string{"ti_abc"}, Classes: []string{"operator"}}}
	job.result.SessionID, job.result.TurnID, job.result.Kind, job.result.Text = "s1", "turn-1", "final", "answer"
	job.result.Runtime, job.result.StopReason = "claude", "end_turn"
	if err := job.journal(s); err != nil {
		t.Fatal(err)
	}
	back, err := readOutputRetry(s, job.journalID)
	if err != nil {
		t.Fatal(err)
	}
	if back.audience == nil || back.audience.Audience != "operator" || back.audience.InputIDs[0] != "ti_abc" || back.audience.Classes[0] != "operator" {
		t.Fatalf("replayed audience: %+v", back.audience)
	}
}

// A mail wake is classified from the principal the hub verified when it stored the
// message, not from the From it claims.
func TestMailWakeInputContextUsesTheVerifiedPrincipal(t *testing.T) {
	s := workerOutputService(t)
	ctx := context.Background()
	send := func(from string, meta map[string]string) string {
		env, err := s.Store.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: addr(t, from), To: addr(t, "msg://agent/agent-mux/agt_target"), Payload: []byte(`{"body":"hi"}`), Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return env.ID
	}
	stamp := func(state, principal string) map[string]string {
		return map[string]string{api.MetaFromVerification: state, api.MetaFromPrincipal: principal}
	}
	for name, tc := range map[string]struct {
		id    string
		class string
		basis string
	}{
		"agent From through the shared operator credential": {send("msg://agent/agent-mux/agt_bl3jn0l3oq", stamp("verified", "msg://user/local/operator")), store.ClassUnknown, store.BasisSharedOperator},
		"session sending from its own address":              {send("msg://session/local/s9", stamp("stamped", "msg://session/local/s9")), store.ClassAgent, store.BasisStampedSender},
		"unverified":                                        {send("msg://user/local/alice", stamp("unverified", "msg://user/local/operator")), store.ClassUnknown, store.BasisNone},
		"unstamped":                                         {send("msg://user/local/alice", nil), store.ClassUnknown, store.BasisNone},
		"message not found":                                 {"no-such-message", store.ClassUnknown, store.BasisNone},
	} {
		t.Run(name, func(t *testing.T) {
			note, ok := turnInputFrom(s.mailWakeInputContext(ctx, tc.id))
			if !ok || note.Source != store.InputMailWake || note.Class != tc.class || note.Basis != tc.basis || note.RefID != tc.id {
				t.Fatalf("%+v ok=%v, want class %s basis %s", note, ok, tc.class, tc.basis)
			}
		})
	}
}

// The creation prompt of a chat-started session is recorded and settled with the launch.
func TestLaunchPromptInputIsRecordedAndSettled(t *testing.T) {
	s := workerOutputService(t)
	s.recordLaunchPromptInput("s1", &identity.InputOrigin{Source: store.InputChatSend, SenderURN: "msg://device/tachyon", Class: store.ClassOperator, Basis: store.BasisPrincipal})
	s.settleLaunchPromptInput("s1", true)
	if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-1", "first reply")}); err != nil {
		t.Fatal(err)
	}
	outs := outputEvents(t, s)
	if len(outs) != 1 || outs[0].Audience != "operator" {
		t.Fatalf("the first reply to a chat-started session must carry the operator audience: %+v", outs)
	}
}

// Publication must never wait on the input table: with storage blocked the bind
// returns within the output persistence deadline as "unknown / binding_unavailable",
// and no input is consumed for it.
func TestAudienceBindingIsBoundedByTheOutputDeadline(t *testing.T) {
	s := workerOutputService(t)
	s.beginTurnInput(deviceCtx("operate"), "s1")(nil)
	s.turnOutputTimeout = 25 * time.Millisecond
	conn, err := s.Store.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := s.audienceFor("s1", "turn-1")
	took := time.Since(start)
	_ = conn.Close()
	if took > time.Second {
		t.Fatalf("binding held the reader for %s", took)
	}
	if got == nil || got.Audience != "unknown" || got.Basis != "binding_unavailable" || len(got.Classes) != 0 || len(got.InputIDs) != 0 {
		t.Fatalf("blocked binding: %+v", got)
	}
	var bound int
	if err := s.Store.DB().QueryRow(`SELECT COUNT(*) FROM turn_inputs WHERE turn_id IS NOT NULL`).Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("a timed-out bind consumed an input: %d %v", bound, err)
	}
}

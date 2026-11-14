package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// The files under docs/fixtures/reply-audience are produced by the real output path
// (record the input, ingest the result, read back the published event and the staged
// routed message) so the examples in the contract cannot drift from the code.
// Regenerate after an intentional change: UPDATE_FIXTURES=1 go test ./internal/app -run ReplyAudienceFixtures
const replyAudienceDir = "../../docs/fixtures/reply-audience"

var idPlaceholder = regexp.MustCompile(`ti_[0-9a-f]{32}`)

type audienceCase struct {
	name  string
	setup func(t *testing.T, s *Service)
}

func mailFixture(t *testing.T, s *Service, from string, meta map[string]string) string {
	t.Helper()
	env, err := s.Store.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: addr(t, from), To: addr(t, "msg://agent/agent-mux/agt_target"), Payload: []byte(`{"body":"hi"}`), Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	return env.ID
}

func triageFixture(t *testing.T, s *Service, messageID, class string) {
	t.Helper()
	if _, _, err := s.Store.TriageRecord(context.Background(), store.TriageRow{MessageID: messageID, Recipient: "msg://agent/agent-mux/agt_target", Seat: "msg://agent/agent-mux/agt_target", Class: class, Reason: "fixture", State: "woken", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestReplyAudienceFixtures(t *testing.T) {
	op := deviceCtx("maintain", "operate", "read")
	cases := []audienceCase{
		{"operator", func(t *testing.T, s *Service) { s.beginTurnInput(op, "s1")(nil) }},
		{"agent", func(t *testing.T, s *Service) {
			// A session principal wrote the mail from its own address; the wake path
			// classifies it from the stored stamp.
			id := mailFixture(t, s, "msg://session/local/s9", map[string]string{api.MetaFromVerification: "stamped", api.MetaFromPrincipal: "msg://session/local/s9"})
			s.beginTurnInput(s.mailWakeInputContext(context.Background(), id), "s1")(nil)
		}},
		{"unknown_shared_operator_credential", func(t *testing.T, s *Service) {
			// An agent-authored mail sent through the shared operator-authorized MCP
			// connection: From is an agent address, the verified principal is the
			// operator credential, which a human or an agent may hold.
			id := mailFixture(t, s, "msg://agent/agent-mux/agt_example", map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: "msg://user/local/operator"})
			s.beginTurnInput(s.mailWakeInputContext(context.Background(), id), "s1")(nil)
		}},
		{"unknown_mail_wake", func(t *testing.T, s *Service) {
			// The case Tachyon folds (CW-20261114-0023b): a cell seat's mail wakes
			// another seat through the shared operator credential. The sender class is
			// unknown, but the binding is proven, so input_sources says mail_wake.
			id := mailFixture(t, s, "msg://agent/agent-mux/agt_example", map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: "msg://user/local/operator"})
			s.beginTurnInput(s.mailWakeInputContext(context.Background(), id), "s1")(nil)
		}},
		{"unknown_mail_wake_action", func(t *testing.T, s *Service) {
			// The same wake on a seat with mail triage on (CW-20261114-0080): the
			// waking mail was triaged action, so input_triage says so.
			id := mailFixture(t, s, "msg://agent/agent-mux/agt_example", map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: "msg://user/local/operator"})
			triageFixture(t, s, id, "action")
			s.beginTurnInput(s.mailWakeInputContext(context.Background(), id), "s1")(nil)
		}},
		{"unknown_mail_wake_human", func(t *testing.T, s *Service) {
			// A wake by mail triaged human (it answers Chrispian): a consumer must
			// never fold this reply.
			id := mailFixture(t, s, "msg://agent/agent-mux/agt_example", map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: "msg://user/local/operator"})
			triageFixture(t, s, id, "human")
			s.beginTurnInput(s.mailWakeInputContext(context.Background(), id), "s1")(nil)
		}},
		{"unknown_shared_operator_credential_direct", func(t *testing.T, s *Service) {
			// /turn or MCP send-turn with the shared operator token: a human at the CLI
			// and a local agent both hold it, so the audience is not proven.
			ctx := identity.WithPrincipal(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Verified: true, Scopes: []string{"*"}})
			s.beginTurnInput(ctx, "s1")(nil)
		}},
		{"system", func(t *testing.T, s *Service) { s.beginTurnInput(deviceCtx("worker"), "s1")(nil) }},
		{"unknown_anonymous", func(t *testing.T, s *Service) { s.beginTurnInput(context.Background(), "s1")(nil) }},
		{"unknown_no_input", func(t *testing.T, s *Service) {}},
		{"unknown_queued_inputs_present", func(t *testing.T, s *Service) {
			s.beginTurnInput(op, "s1")(nil)
			s.beginTurnInput(op, "s1")(nil)
		}},
		{"unknown_submission_unsettled", func(t *testing.T, s *Service) { _ = s.beginTurnInput(op, "s1") }},
		{"unknown_failed_input_ignored", func(t *testing.T, s *Service) { s.beginTurnInput(op, "s1")(os.ErrClosed) }},
	}
	names := make([]string, 0, len(cases))
	for _, c := range cases {
		names = append(names, c.name)
		t.Run(c.name, func(t *testing.T) {
			s := workerOutputService(t)
			c.setup(t, s)
			if err := s.ingestWorkerFrame("worker", workerwire.Frame{T: "output", Session: "s1", Seq: 1, Stream: "stdout", Data: resultLine("turn-1", "the reply")}); err != nil {
				t.Fatal(err)
			}
			outs := outputEvents(t, s)
			if len(outs) != 1 {
				t.Fatalf("outputs: %+v", outs)
			}
			staged, err := s.Store.StagedTurnOutput(context.Background(), outs[0].MessageID)
			if err != nil {
				t.Fatal(err)
			}
			meta := map[string]string{}
			for k, v := range staged.Metadata {
				switch k {
				case "audience", "audience_basis", "input_ids", "input_classes", "input_sources", "input_triage", "kind", "session_id", "turn_id":
					meta[k] = idPlaceholder.ReplaceAllString(v, "ti_example")
				}
			}
			event := outs[0]
			event.MessageID, event.OutputID, event.ProviderResultID = "msg_example", "out_example", "turn-1"
			for i := range event.InputIDs {
				event.InputIDs[i] = "ti_example"
			}
			body, err := json.MarshalIndent(map[string]any{
				"note":             "event is the session.turn_output payload; message_metadata is the audience part of the routed message metadata. Ids are replaced by stable placeholders.",
				"event":            event,
				"message_metadata": meta,
			}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			goldenReply(t, c.name+".json", append(body, '\n'))
			checkAudienceInvariants(t, event, meta)
		})
	}
	sort.Strings(names)
	index, _ := json.MarshalIndent(map[string]any{"cases": names, "generated_by": "TestReplyAudienceFixtures"}, "", "  ")
	goldenReply(t, "index.json", append(index, '\n'))
}

// The invariants the consumer contract states, enforced on every fixture.
func checkAudienceInvariants(t *testing.T, e events.TurnOutputEvent, meta map[string]string) {
	t.Helper()
	switch e.Audience {
	case "operator", "agent", "system":
		if len(e.InputClasses) != 1 || e.InputClasses[0] != e.Audience || len(e.InputIDs) != 1 {
			t.Fatalf("a proven audience must carry exactly its class and one input: %+v", e)
		}
	case "unknown":
		if len(e.InputClasses) != 0 {
			t.Fatalf("an unknown audience must expose no class: %+v", e)
		}
	default:
		t.Fatalf("audience %q is not in the vocabulary", e.Audience)
	}
	if e.AudienceBasis == "" || meta["audience"] != e.Audience || meta["audience_basis"] != e.AudienceBasis {
		t.Fatalf("event and routed metadata disagree: %+v %v", e, meta)
	}
	if _, has := meta["input_classes"]; has != (len(e.InputClasses) > 0) {
		t.Fatalf("input_classes present in metadata iff classes are proven: %v", meta)
	}
	if len(e.InputSources) > 0 {
		if len(e.InputSources) != len(e.InputIDs) {
			t.Fatalf("input_sources must pair with input_ids: %+v", e)
		}
		for _, src := range e.InputSources {
			if !slices.Contains(inputSourceVocabulary, src) {
				t.Fatalf("input source %q is not in the turn_inputs.source vocabulary", src)
			}
		}
		switch e.AudienceBasis {
		case "submission_unsettled", "queued_input", "queued_inputs_present", "binding_unavailable", "no_input":
			t.Fatalf("an unproven binding must expose no source: %+v", e)
		}
	}
	if meta["input_sources"] != strings.Join(e.InputSources, ",") {
		t.Fatalf("event and routed metadata disagree on input_sources: %+v %v", e, meta)
	}
	if len(e.InputTriage) > 0 {
		if len(e.InputTriage) != len(e.InputIDs) || len(e.InputSources) != len(e.InputIDs) {
			t.Fatalf("input_triage needs a proven binding and must pair with input_ids: %+v", e)
		}
		blank := true
		for i, class := range e.InputTriage {
			if !slices.Contains([]string{"human", "action", "fyi", "read", ""}, class) {
				t.Fatalf("input triage %q is not in the vocabulary", class)
			}
			if class != "" && e.InputSources[i] != store.InputMailWake {
				t.Fatalf("a triage class on an input that is not a mail wake: %+v", e)
			}
			blank = blank && class == ""
		}
		if blank {
			t.Fatalf("input_triage must be omitted when every position is empty: %+v", e)
		}
	}
	if meta["input_triage"] != strings.Join(e.InputTriage, ",") {
		t.Fatalf("event and routed metadata disagree on input_triage: %+v %v", e, meta)
	}
}

var inputSourceVocabulary = []string{store.InputChatSend, store.InputSessionTurn, store.InputMCPSendTurn, store.InputMailWake, store.InputRoutingReply, store.InputSystem}

func goldenReply(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join(replyAudienceDir, name)
	if os.Getenv("UPDATE_FIXTURES") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s missing; regenerate with UPDATE_FIXTURES=1: %v", name, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("%s differs from the code's output; regenerate with UPDATE_FIXTURES=1 if intended.\nwant:\n%s\ngot:\n%s", name, want, got)
	}
}

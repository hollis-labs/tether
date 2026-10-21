package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
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
				case "audience", "audience_basis", "input_ids", "input_classes", "kind", "session_id", "turn_id":
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
}

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

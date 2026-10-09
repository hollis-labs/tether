package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestHostedOutputAcceptanceReplaysAndVerifiesInsideReceiptTransaction(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrouted", true: "selected"}[selected], func(t *testing.T) {
			var route *launchprofile.Route
			if selected {
				route = &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}}
			}
			svc, output := outputHarness(t, route)
			svc.turnOutputs.Store("s1", output)
			svc.MarkTurnOutputSessionLost("s1")
			result := turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: "actual completed output", Runtime: "codex"}
			messageID, outputID, err := svc.persistHostedTurnOutput(context.Background(), "s1", result, "native-source")
			if err != nil || outputID == "" || selected != (messageID != "") {
				t.Fatal("hosted output was not durably accepted", err)
			}
			repeated, repeatedID, err := svc.persistHostedTurnOutput(context.Background(), "s1", result, "native-source")
			if err != nil || repeated != messageID || repeatedID != outputID {
				t.Fatal("hosted replay lost stable acceptance", err)
			}
			if got := outputEvents(t, svc); len(got) != 1 || !got[0].FreshConversation {
				t.Fatal("hosted replay duplicated output or lost continuity marker")
			}
			tx, err := svc.Store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := svc.Store.VerifyTurnOutputAcceptance(context.Background(), tx, "s1", outputID, messageID, result.TurnID); err != nil {
				t.Fatal(err)
			}
			if err := svc.Store.VerifyTurnOutputAcceptance(context.Background(), tx, "s1", outputID, messageID, "wrong-turn"); err == nil {
				t.Fatal("receipt accepted wrong native turn")
			}
		})
	}
}

func TestHostedOutputStageFailureCannotCertifyDelivery(t *testing.T) {
	svc, _ := outputHarness(t, &launchprofile.Route{Channel: "ops", Kinds: []string{"final"}})
	if _, err := svc.Store.DB().Exec(`CREATE TRIGGER fail_hosted_stage BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL,'fixture stage failure'); END`); err != nil {
		t.Fatal(err)
	}
	result := turnoutput.Output{TurnID: "native-turn", Kind: turnoutput.KindFinal, Text: "answer"}
	messageID, outputID, err := svc.persistHostedTurnOutput(context.Background(), "s1", result, "native-source")
	if err == nil || messageID != "" || outputID == "" {
		t.Fatal("failed stage reported delivery acceptance")
	}
	tx, err := svc.Store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := svc.Store.VerifyTurnOutputAcceptance(context.Background(), tx, "s1", outputID, messageID, result.TurnID); err == nil {
		t.Fatal("receipt certified absent output")
	}
}

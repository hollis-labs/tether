package app

import (
	"context"
	"testing"

	agentsessions "github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261021-0029 (found live): a wake the sweep retries after a busy first attempt is the same
// mail wake, so its recorded input carries the message and the sender the hub stamped, exactly as
// a first-attempt wake does. It was recorded as a generic session_turn with no message id.
func TestSweepRetriedWakeKeepsItsMailProvenance(t *testing.T) {
	const agentPrincipal = "msg://agent/agent-mux/agt_sender"
	for name, tc := range map[string]struct {
		meta  map[string]string
		class string
		basis string
	}{
		"agent principal":            {map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: agentPrincipal}, store.ClassAgent, store.BasisStampedSender},
		"shared operator credential": {map[string]string{api.MetaFromVerification: "verified", api.MetaFromPrincipal: "msg://user/local/operator"}, store.ClassUnknown, store.BasisSharedOperator},
		"unstamped":                  {nil, store.ClassUnknown, store.BasisNone},
	} {
		t.Run(name, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			ctx := context.Background()
			svc := &Service{Store: st}
			rt := newFakeRuntime()
			rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
			seam := rt.seam()
			plain := seam.sendTurn
			// Record the input the way the real SendTurn does, so the row is what production writes.
			seam.sendTurn = func(c context.Context, sessionID, text string) error {
				settle := svc.beginTurnInput(c, sessionID)
				err := plain(c, sessionID, text)
				settle(err)
				return err
			}
			to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
			env, err := st.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice,
				From: messaging.Address{Kind: messaging.KindAgent, Authority: "agent-mux", ID: "agt_sender"}, To: to, Metadata: tc.meta})
			if err != nil {
				t.Fatal(err)
			}
			if out := attemptWake(svc.mailWakeInputContext(ctx, env.ID), st, reg, seam, env.ID, to, "s1", "wake up"); out.Reason != "busy" {
				t.Fatalf("first attempt = %+v; want busy", out)
			}
			makeDue(t, st, deliveryIDOf(t, st, env.ID))
			rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			if n, err := runWakeSweep(ctx, st, reg, seam, nil); err != nil || n != 1 {
				t.Fatalf("sweep attempted %d (err %v); want the one retry", n, err)
			}
			var source, class, basis, ref string
			if err := st.DB().QueryRow(`SELECT source, sender_class, basis, COALESCE(ref_id,'') FROM turn_inputs WHERE session_id='s1'`).Scan(&source, &class, &basis, &ref); err != nil {
				t.Fatalf("the retried wake recorded no input: %v", err)
			}
			if source != store.InputMailWake || ref != env.ID || class != tc.class || basis != tc.basis {
				t.Fatalf("retried wake input = source %q ref %q class %q basis %q; want mail_wake, %s, %s, %s", source, ref, class, basis, env.ID, tc.class, tc.basis)
			}
		})
	}
}

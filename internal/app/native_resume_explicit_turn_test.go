package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
)

type nativeOnlyTurnSession struct {
	agentsessions.Session
	calls []string
}

func (s *nativeOnlyTurnSession) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	s.calls = append(s.calls, method)
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var request struct {
		ThreadID string `json:"threadId"`
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	if method != "turn/start" || request.ThreadID != "thread-old" {
		return nil, errors.New("unexpected thread allocation or binding")
	}
	return json.RawMessage(`{}`), nil
}

type nativeOnlyTurnRuntime struct {
	agentsessions.Runtime
	session *nativeOnlyTurnSession
}

func (r nativeOnlyTurnRuntime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{JsonRpcStdio: true}
}
func (r nativeOnlyTurnRuntime) Start(context.Context, agentsessions.StartOptions) (agentsessions.Session, error) {
	return r.session, nil
}

func TestNativeOnlyExplicitTurnUsesAlreadyBoundThread(t *testing.T) {
	ctx := context.Background()
	r := recoveryRuntimeRig(t, nil)
	if _, err := r.service.Store.DB().Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$.native_resume_only',json('true')) WHERE session_id='resumed'`); err != nil {
		t.Fatal(err)
	}
	if err := r.service.Store.UpsertSessionProviderMapping("resumed", "tether", r.plan.ProviderID, "thread-old"); err != nil {
		t.Fatal(err)
	}
	runtime, err := stub.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := runtime.Start(ctx, agentsessions.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rpc := &nativeOnlyTurnSession{Session: child}
	r.service.Manager = agentsessions.NewManager(nil)
	t.Cleanup(func() { _ = rpc.Stop(ctx); _ = r.service.Manager.Shutdown(ctx) })
	if err := r.service.Manager.Start(ctx, agentsessions.StartRequest{ID: "resumed", Runtime: nativeOnlyTurnRuntime{Runtime: runtime, session: rpc}}); err != nil {
		t.Fatal(err)
	}
	if err := r.service.sendTurnJSONRPC(ctx, "resumed", "explicit request"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rpc.calls, []string{"turn/start"}) {
		t.Fatal("explicit turn initialized or created a thread", rpc.calls)
	}
	if err := r.service.Store.UpsertSessionProviderMapping("resumed", "tether", r.plan.ProviderID, "foreign"); err != nil {
		t.Fatal(err)
	}
	if err := r.service.sendTurnJSONRPC(ctx, "resumed", "must refuse"); !errors.Is(err, ErrNativeOnlyUnavailable) {
		t.Fatal("changed binding accepted", err)
	}
	if len(rpc.calls) != 1 {
		t.Fatal("changed mapping dispatched RPC")
	}
}

//go:build !windows

package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// These are current-contract refusal fixtures. Private protocol obligations do
// not issue a delivery proof or authorize a replacement Codex controller.
func TestTeamCodexCustodyRefusesDirectReplacement(t *testing.T) {
	for _, tc := range []struct {
		name, state, fence string
		gone               bool
	}{
		{name: "failed_minus_one", state: "failed"},
		{name: "orphaned", state: "orphaned"},
		{name: "gone_host_and_provider_failed", state: "failed", gone: true},
		{name: "gone_host_and_provider_orphaned", state: "orphaned", gone: true},
		{name: "revoked_binding", state: "failed", fence: `UPDATE runtime_bindings SET revoked_at='fixture-revoked' WHERE host_id='team'`},
		{name: "explicit_stop_intent", state: "failed", fence: `UPDATE team_port_intents SET ended='stop' WHERE port_kind='session' AND intent_key='retained'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvLaunchHost, string(HostDirect))
			r, id, actor := retainedRecoveryRig(t)
			ctx := context.Background()
			// A restarted daemon has no live controller for the retained session.
			r.svc.Manager = agentsessions.NewManager(nil)
			if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET state=?,exit_code=-1 WHERE id=?`, tc.state, id); err != nil {
				t.Fatal(err)
			}
			receipt, protocol := seedTeamCodexCustody(t, r, id)
			if tc.fence != "" {
				if _, err := r.svc.Store.DB().Exec(tc.fence); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotTeamCodexCustody(t, r, id, actor, protocol)
			starts := r.count()
			if tc.gone {
				t.Setenv(EnvLaunchHost, string(HostShim))
				inspected := false
				r.svc.shimHosting = &shimHosting{
					inspect: func(_ context.Context, observed shimhost.Receipt) (shimhost.Inspection, error) {
						inspected = true
						if observed != receipt {
							t.Error("inspection used a replacement custody receipt")
						}
						return shimhost.Inspection{Receipt: observed, Gone: true, Running: false}, nil
					},
					stop: func(context.Context, shimhost.Receipt) error {
						t.Error("Gone observation retired unresolved Codex custody")
						return errors.New("fixture refuses custody retirement")
					},
				}
				if !r.svc.reconcileShimContext(ctx, store.StaleSession{ID: id, State: tc.state}) || !inspected {
					t.Fatal("tracked custody did not consume the Gone inspection")
				}
			}
			if err := r.svc.RecoverTeamSession(ctx, "retained", id); !errors.Is(err, teamhost.ErrSessionUnavailable) {
				t.Fatalf("tracked Codex recovery = %v, want unavailable", err)
			}
			if r.count() != starts {
				t.Fatal("refused recovery started a replacement provider")
			}
			after := snapshotTeamCodexCustody(t, r, id, actor, protocol)
			for key, expected := range before {
				if !reflect.DeepEqual(after[key], expected) {
					t.Errorf("refused recovery changed %s", key)
				}
			}
		})
	}
}

type custodyResumeRuntime struct {
	agentsessions.Runtime
	starts *atomic.Int32
}

func (r custodyResumeRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	r.starts.Add(1)
	return r.Runtime.Start(ctx, opts)
}

func TestLogicalResumeRefusesTrackedCodexCustody(t *testing.T) {
	for _, state := range []string{"failed", "orphaned"} {
		t.Run(state, func(t *testing.T) {
			r := newCodexRig(t)
			id := r.start()
			if err := r.turn(id, "initial work"); err != nil {
				t.Fatal(err)
			}
			r.wait(1)
			r.idle(id)
			if err := r.svc.StopSession(id); err != nil {
				t.Fatal(err)
			}
			r.ended(id)
			if _, err := r.svc.Store.DB().Exec(`UPDATE sessions SET state=?,exit_code=-1 WHERE id=?`, state, id); err != nil {
				t.Fatal(err)
			}
			_, protocol := seedTeamCodexCustody(t, r, id)
			ctx := context.Background()
			custody, _ := r.svc.Store.SessionShim(ctx, id)
			mapping, _ := r.svc.Store.GetSessionProviderMapping(id, "tether", "codex-cli")
			obligations, err := protocol.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var starts atomic.Int32
			factory := r.svc.factories["codex-cli"]
			r.svc.factories["codex-cli"] = func(plan *launch.Plan) (agentsessions.Runtime, error) {
				runtime, err := factory(plan)
				return custodyResumeRuntime{Runtime: runtime, starts: &starts}, err
			}
			if _, err := r.svc.ResumeLogicalAgent("agent", api.ResumeOptions{}); !errors.Is(err, session.ErrRecoveryConflict) {
				t.Fatalf("logical resume bypassed custody: %v", err)
			}
			latest, _ := r.svc.Store.LatestSessionForAgent(ctx, "agent")
			if latest == nil || latest.ID != id || r.count() != 1 || starts.Load() != 0 {
				t.Fatal("refused resume allocated a replacement canonical session or provider")
			}
			afterCustody, _ := r.svc.Store.SessionShim(ctx, id)
			afterMapping, _ := r.svc.Store.GetSessionProviderMapping(id, "tether", "codex-cli")
			afterObligations, err := protocol.Load(ctx)
			if err != nil || custody != afterCustody || mapping != afterMapping || !reflect.DeepEqual(obligations, afterObligations) {
				t.Fatal("refused logical resume changed custody, native mapping or private obligations", err)
			}
		})
	}
}

func seedTeamCodexCustody(t *testing.T, r *codexRig, id string) (shimhost.Receipt, *store.CodexProtocolStore) {
	t.Helper()
	ctx := context.Background()
	mapping, err := r.svc.Store.GetSessionProviderMapping(id, "tether", "codex-cli")
	if err != nil || !mapping.NativeSessionID.Valid || mapping.NativeSessionID.String == "" {
		t.Fatalf("fixture requires an observed native mapping: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "custody")
	if err := shimhost.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	receipt := shimhost.Receipt{
		Session: id, Instance: "custody-fixture", Generation: 7, OperationKey: id + ":7",
		DescriptorPath: filepath.Join(dir, "launch.json"), SocketPath: filepath.Join(dir, "control.sock"),
		Backend: shimhost.Detached, Journal: "custody-journal", Fingerprint: "fixture-fingerprint",
		SubmissionAttemptID: "fixture-attempt", Attempted: true,
	}
	if err := shimhost.WritePrivateJSON(filepath.Join(dir, "placement.json"), receipt); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.persistShim(ctx, id, "codex-app", "fixture-boot", receipt); err != nil {
		t.Fatal(err)
	}
	protocol, err := r.svc.Store.CodexProtocolStore(ctx, id, receipt.OperationKey, codexRecordLimit)
	if err != nil {
		t.Fatal(err)
	}
	state := shimcodex.State{
		Version: shimcodex.Version, Binding: codexBinding(receipt), Revision: 1, Epoch: 1,
		NextID: shimcodex.FirstID + 1, ThreadID: mapping.NativeSessionID.String,
		Partial:    []byte("unfinished private protocol fragment"),
		Operations: []shimcodex.Operation{{ID: shimcodex.FirstID, Method: "turn/start", Params: []byte(`{}`), Phase: shimcodex.Attempted, EffectUnknown: true}},
	}
	if err := protocol.Commit(ctx, 0, state); err != nil {
		t.Fatal(err)
	}
	return receipt, protocol
}

func snapshotTeamCodexCustody(t *testing.T, r *codexRig, id, actor string, protocol *store.CodexProtocolStore) map[string]any {
	t.Helper()
	ctx := context.Background()
	row, err := r.svc.Store.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	shim, err := r.svc.Store.SessionShim(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := r.svc.Store.GetSessionProviderMapping(id, "tether", "codex-cli")
	if err != nil {
		t.Fatal(err)
	}
	state, err := protocol.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := map[string]any{"session": *row, "shim custody": shim, "native mapping": mapping, "private obligations": state}
	for key, query := range map[string]string{
		"session identities":           `SELECT json_group_array(id) FROM (SELECT id FROM sessions ORDER BY id)`,
		"actor":                        `SELECT json_object('urn',urn,'kind',kind,'status',status,'props',props_json) FROM registry_entries WHERE urn=?`,
		"host ownership":               `SELECT json_object('request',CAST(request AS TEXT),'member',CAST(member AS TEXT),'tombstone',tombstone,'dead',dead,'cleaned',cleaned) FROM team_host_intents WHERE intent_key='retained'`,
		"enrollment and stop intent":   `SELECT json_group_array(json_object('kind',port_kind,'state',state,'request',CAST(request AS TEXT),'payload',CAST(payload AS TEXT),'actor',acquired_urn,'binding',binding_secret,'ended',ended,'binding_ended',binding_ended)) FROM (SELECT * FROM team_port_intents WHERE intent_key='retained' ORDER BY port_kind)`,
		"binding including revocation": `SELECT json_object('id',id,'actor',target_urn,'session',session_id,'generation',generation,'attempt',attempt_id,'host',host_id,'visibility',visibility,'revoked',revoked_at,'lease',lease_expires_at) FROM runtime_bindings WHERE target_urn=? AND host_id='team'`,
	} {
		var value string
		var args []any
		if key == "actor" || key == "binding including revocation" {
			args = []any{actor}
		}
		if err := r.svc.Store.DB().QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
			t.Fatal(err)
		}
		snapshot[key] = value
	}
	return snapshot
}

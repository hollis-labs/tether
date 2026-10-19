package events

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/libs/harness/core/eventstage"
	"github.com/hollis-labs/libs/security/redact"
)

func pipelineFixture(t *testing.T, mode StageMode, e Event) (Event, *Pipeline) {
	t.Helper()
	p, err := NewPipeline(mode)
	if err != nil {
		t.Fatal(err)
	}
	persisted := &envPersister{}
	bus := NewBus(BusOptions{Persister: persisted, Pipeline: p})
	if err := bus.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return persisted.last(), p
}

func TestPipelineMetadataGoldensAndCompatibilityBytes(t *testing.T) {
	fixtures := map[string]string{
		KindSessionUsage:                  `{"input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_write_tokens":4,"reasoning_tokens":5,"turns":1,"model":"fixture-model","final":true,"source":"provider","turn_id":"turn-1"}`,
		KindSessionHealthChanged:          `{"from":"unknown","to":"healthy","reasons":["session_running"],"since":"2026-10-19T00:00:00Z"}`,
		catalog.KindToolCallStarted:       `{"tool":"read","server":"fixture","args_bytes":2,"args_schema_fp":"1234"}`,
		catalog.KindToolCallFinished:      `{"tool":"read","server":"fixture","args_bytes":2,"args_schema_fp":"1234","ok":true,"error_class":"none","duration_ms":2,"gateway_ms":1,"forward_ms":1,"result_bytes":2}`,
		KindSessionStalled:                `{"reason":"repeated_tool_denials","denied":3}`,
		EventTypeMachineAuthorityDecision: `{"server":"fixture","tool":"write","class":"write","mode":"dev","allowed":true,"session_id":"s1","timestamp":"2026-10-19T00:00:00Z"}`,
	}
	for kind, policy := range pipelinePolicies() {
		t.Run(kind, func(t *testing.T) {
			raw := fixtures[kind]
			if policy.class == Metadata && raw == "" {
				t.Fatal("metadata policy has no golden consumer fixture")
			}
			if policy.class == Compatibility {
				raw = " {\"text\":\"hello\",\"contact\":\"synthetic@example.com\"} "
			}
			for _, mode := range []StageMode{Shadow, Enforce} {
				original := Event{Scope: ScopeSession, SessionID: "s1", Kind: kind, PayloadJSON: raw, Envelope: Envelope{Dims: ScopeDims{TaskID: "CW-1"}, Actor: Actor{Kind: "agent", URN: "msg://agent/fixture/one", Verified: "token"}}}
				got, _ := pipelineFixture(t, mode, original)
				if policy.class == Compatibility || mode == Shadow {
					if got.PayloadJSON != raw || got.Envelope.Class.Sanitized {
						t.Fatalf("compatibility changed in %s", mode)
					}
				} else {
					var before, after any
					if json.Unmarshal([]byte(raw), &before) != nil || json.Unmarshal([]byte(got.PayloadJSON), &after) != nil || !reflect.DeepEqual(before, after) || !got.Envelope.Class.Sanitized {
						t.Fatalf("metadata golden changed: %s", got.PayloadJSON)
					}
				}
				if !reflect.DeepEqual(got.Envelope.Actor, original.Envelope.Actor) || got.Envelope.Dims != original.Envelope.Dims {
					t.Fatal("sanitize changed verified facts")
				}
			}
		})
	}
}

func TestPipelineMasksMetadataAndNeverClaimsShadowEnforcement(t *testing.T) {
	original := Event{Scope: ScopeSession, Kind: catalog.KindToolCallFinished, PayloadJSON: `{"tool":"synthetic@example.com","ok":true,"password":"SYNTHETIC_PASSWORD","actor":{"verified":"token"},"task_id":"forged"}`}
	got, p := pipelineFixture(t, Enforce, original)
	if strings.Contains(got.PayloadJSON, "synthetic@example.com") || strings.Contains(got.PayloadJSON, "SYNTHETIC_PASSWORD") || strings.Contains(got.PayloadJSON, "forged") || !got.Envelope.Class.Sanitized || !got.Envelope.Class.PII || got.Envelope.Class.Redactions < 1 {
		t.Fatalf("metadata not sanitized: %s %+v", got.PayloadJSON, got.Envelope.Class)
	}
	shadow, sp := pipelineFixture(t, Shadow, original)
	if shadow.PayloadJSON != original.PayloadJSON || !reflect.DeepEqual(shadow.Envelope.Class, original.Envelope.Class) {
		t.Fatal("shadow changed local contract")
	}
	for _, counts := range []map[string]StageCounts{p.Counts(), sp.Counts()} {
		c := counts[original.Kind]
		if c.Events != 1 || c.Changed != 1 || c.Redactions < 1 {
			t.Fatalf("missing diagnostic counts: %+v", c)
		}
		raw, _ := json.Marshal(counts)
		if strings.Contains(string(raw), "synthetic@example.com") || strings.Contains(string(raw), "SYNTHETIC_PASSWORD") {
			t.Fatal("diagnostics retained values")
		}
	}
}

type panicPayloadSanitizer struct{}

func (panicPayloadSanitizer) SanitizePayload(context.Context, string, []byte) ([]byte, eventstage.PayloadReport, error) {
	panic("SYNTHETIC_PRIVATE_PANIC")
}

func TestPipelineFailsClosedWithoutLosingEvent(t *testing.T) {
	for name, raw := range map[string]string{"invalid_json": "{", "oversized": strings.Repeat("x", redact.DefaultMaxBytes+1), "tombstone_with_payload": `{"input_tokens":1}`, "redactions_overflow": `{"input_tokens":1}`, "panic": `{"input_tokens":1}`, "opaque": `{"input_tokens":1}`} {
		t.Run(name, func(t *testing.T) {
			p, err := NewPipeline(Enforce)
			if name == "panic" {
				p, err = newPipeline(Enforce, panicPayloadSanitizer{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "opaque" {
				sanitizer, err := redact.NewSanitizer(redact.Policy{Format: redact.Opaque})
				if err != nil {
					t.Fatal(err)
				}
				p.policies[KindSessionUsage] = admissionPolicy{class: Metadata, sanitizer: sanitizer}
			}
			original := Event{Scope: ScopeSession, Kind: KindSessionUsage, PayloadJSON: raw}
			if name == "tombstone_with_payload" {
				original.Envelope.PayloadDigest = "sha256:fixture"
			}
			if name == "redactions_overflow" {
				original.Envelope.Class.Redactions = int(uint64(math.MaxUint32) + 1)
			}
			got := p.apply(context.Background(), original, func(_ context.Context, e Event) Event { return e })
			if got.PayloadJSON == raw || !strings.Contains(got.PayloadJSON, "sanitize_error") || strings.Contains(got.PayloadJSON, "PRIVATE") || got.Envelope.Class.Sanitized || got.Kind != original.Kind {
				t.Fatalf("fail-open: %s", got.PayloadJSON)
			}
			if p.Counts()[original.Kind].Failures != 1 {
				t.Fatal("failure not counted")
			}
		})
	}
}

func TestPipelineUnknownKindsAndBoundedCounters(t *testing.T) {
	for _, mode := range []StageMode{Shadow, Enforce} {
		for _, raw := range []string{`{"private":"synthetic"}`, ""} {
			original := Event{Scope: ScopeSession, Kind: "unknown-private-kind", PayloadJSON: raw}
			got, p := pipelineFixture(t, mode, original)
			if mode == Shadow && got.PayloadJSON != raw {
				t.Fatal("shadow changed unknown")
			}
			if mode == Enforce && got.PayloadJSON != `{"sanitize_error":"unknown_kind"}` {
				t.Fatalf("unknown enforce=%s", got.PayloadJSON)
			}
			if got.Envelope.Class.Sanitized || len(p.Counts()) != 1 || p.Counts()["<unregistered>"].Class != Unregistered {
				t.Fatal("unbounded or incorrect diagnostics")
			}
		}
	}
	if _, err := NewPipeline("invalid"); err == nil {
		t.Fatal("invalid startup mode accepted")
	}
}

func TestPipelinePreservesStampIsolationAndTxSelection(t *testing.T) {
	p, err := NewPipeline(Enforce)
	if err != nil {
		t.Fatal(err)
	}
	store := &envPersister{}
	var safeCalls, unsafeCalls int
	bad := StageFunc(func(_ context.Context, e *Event) error {
		e.Envelope.Class.Labels[0] = "partial"
		return errors.New("stamp failed")
	})
	bus := NewBus(BusOptions{Persister: store, Pipeline: p, Stages: []Stage{bad, txSafeStage{&safeCalls}, StageFunc(func(context.Context, *Event) error { unsafeCalls++; return nil })}, OnStageError: func(*StageError) {}})
	original := Event{Scope: ScopeSession, Kind: KindSessionUsage, PayloadJSON: `{"input_tokens":1}`, Envelope: Envelope{Class: Class{Labels: []string{"keep"}}}}
	if err := bus.Publish(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	got := store.last()
	if got.Envelope.Class.Labels[0] != "keep" || original.Envelope.Class.Labels[0] != "keep" || !got.Envelope.Class.Sanitized || safeCalls != 1 || unsafeCalls != 1 {
		t.Fatal("stamp isolation/order changed")
	}
}

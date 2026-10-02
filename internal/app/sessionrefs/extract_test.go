package sessionrefs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
)

type capture struct {
	ids                 []string
	refs                []Ref
	sessionIDs, sources []string
	err                 error
}

func (s *capture) AttachSessionRef(_ context.Context, sessionID, kind, id, uri, relation, source, parent string) error {
	s.ids = append(s.ids, id)
	s.refs = append(s.refs, Ref{Kind: kind, RefID: id, URI: uri, Relation: relation, ParentItemID: parent})
	s.sessionIDs = append(s.sessionIDs, sessionID)
	s.sources = append(s.sources, source)
	return s.err
}

type resolver struct {
	calls  int
	result *ResolvedRef
	err    error
}

func (r *resolver) ResolveRef(context.Context, map[string]any) (*ResolvedRef, error) {
	r.calls++
	return r.result, r.err
}
func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRecordGatesBeforeResolutionAndAttachment(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled         bool
		sessionID       string
		nilSink, failed bool
	}{
		{name: "disabled", sessionID: "own"}, {name: "missing session", enabled: true}, {name: "missing sink", enabled: true, sessionID: "own", nilSink: true}, {name: "failed call", enabled: true, sessionID: "own", failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &capture{}
			res := &resolver{}
			var attacher Attacher = sink
			if tc.nilSink {
				attacher = nil
			}
			s := New(tc.enabled, tc.sessionID, res, attacher, testLogger())
			s.Record(context.Background(), Observation{ToolName: "memory_write", Arguments: map[string]any{"namespace": "ns", "key": "key"}, Failed: tc.failed})
			if res.calls != 0 || len(sink.refs) != 0 {
				t.Fatalf("resolver calls=%d refs=%v", res.calls, sink.refs)
			}
		})
	}
}

func TestRecordTypedIdentityReplayAndPreviewPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, tool string
		result     map[string]any
		want       []Ref
	}{
		{name: "declared revision", tool: "memory_write", result: map[string]any{"revision_id": "revision", "item_id": "item"}, want: []Ref{{Kind: KindTesseractRevision, RefID: "revision", URI: "tesseract://revision/revision", Relation: RelationCreated, ParentItemID: "item"}}},
		{name: "workspace replay", tool: "workspace_write", result: map[string]any{"status": "replayed", "item_id": "item"}},
		{name: "search preview", tool: "tesseract_recall", result: map[string]any{"item_id": "item"}},
		{name: "resolution preview", tool: "tesseract_ref_resolve", result: map[string]any{"item_id": "item"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &capture{}
			s := New(true, "own", nil, sink, testLogger())
			s.Record(context.Background(), Observation{ToolName: tc.tool, Result: tc.result})
			if !reflect.DeepEqual(sink.refs, tc.want) {
				t.Fatalf("refs=%+v want=%+v", sink.refs, tc.want)
			}
			for i := range sink.refs {
				if sink.sessionIDs[i] != "own" || sink.sources[i] != "proxy" {
					t.Fatalf("session/source=%v/%v", sink.sessionIDs, sink.sources)
				}
			}
		})
	}
}

func TestRecordFailedResolverPreservesUnresolvedCapture(t *testing.T) {
	sink := &capture{}
	res := &resolver{err: errors.New("offline")}
	s := New(true, "own", res, sink, testLogger())
	s.Record(context.Background(), Observation{ToolName: "memory_write", Arguments: map[string]any{"namespace": "ns", "key": "key"}})
	want := []Ref{{Kind: KindTesseractKey, RefID: "ns:key", Relation: RelationCreated}}
	if res.calls != 1 || !reflect.DeepEqual(sink.refs, want) {
		t.Fatalf("calls=%d refs=%+v", res.calls, sink.refs)
	}
}

func TestRecordAttachErrorsContinueAndScanRefusalAttachesNothing(t *testing.T) {
	sink := &capture{err: errors.New("offline")}
	s := New(true, "own", nil, sink, testLogger())
	s.Record(context.Background(), Observation{ToolName: "torque_task_get", Arguments: map[string]any{"one": "CW-20261001-0546", "two": "CW-20261001-0543"}})
	if len(sink.ids) != 2 {
		t.Fatalf("attach failed to continue: %v", sink.ids)
	}
	wide := make([]any, MaxScanValues+1)
	for i := range wide {
		wide[i] = "CW-20261001-0546"
	}
	s.Record(context.Background(), Observation{ToolName: "torque_task_get", Arguments: map[string]any{"wide": wide}})
	if len(sink.ids) != 2 {
		t.Fatalf("refused scan partially attached: %v", sink.ids)
	}
}

package agentevents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

func newRecorder(t *testing.T, dir, session string) *Recorder {
	t.Helper()
	return &Recorder{App: "tether", SessionID: session, Sink: &Spool{Dir: dir},
		Process: runtimeevents.Process{PID: 42, Provider: "claude", Runtime: "hooks", ProviderSessionID: "native-1"}}
}

func TestRecordStampsTheV11EnvelopeAndRoundTripsToMesh(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir, "sess-1")
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ev, err := r.Record(context.Background(), Observation{
		Kind: runtimeevents.KindAgentToolUse, Channel: runtimeevents.ChannelHook, Tier: Exact, Time: at, TurnID: "turn-1",
		Payload: map[string]any{"tool": "Read"}, CausationID: "evt_cause", ActorURN: "msg://agent/local/claude", ActorKind: "agent", ActorRole: "hook",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ev.SchemaVersion != "1" || ev.Sequence != 1 || ev.ID == "" || ev.Subject != "msg://session/local/sess-1" || ev.SessionID != "sess-1" ||
		!ev.Time.Equal(at) || ev.Source.Confidence != runtimeevents.ConfidenceExact || ev.Source.Channel != runtimeevents.ChannelHook ||
		ev.Corr == nil || ev.Corr.CausationID != "evt_cause" || ev.Actor == nil || ev.Actor.Role != "hook" || ev.Process.Provider != "claude" {
		t.Fatalf("envelope = %+v", ev)
	}
	got, err := (&Spool{Dir: dir}).Read("sess-1")
	if err != nil || len(got) != 1 || got[0].ID != ev.ID || string(got[0].Payload) != `{"tool":"Read"}` {
		t.Fatalf("spooled = %+v err=%v", got, err)
	}
	// The lossless projection to the mesh envelope and back keeps what an adapter set.
	m, err := runtimeevents.ToMesh(got[0])
	if err != nil {
		t.Fatalf("ToMesh: %v", err)
	}
	back, err := runtimeevents.FromMesh(m)
	if err != nil {
		t.Fatalf("FromMesh: %v", err)
	}
	if back.ID != ev.ID || back.Kind != ev.Kind || back.SessionID != ev.SessionID || back.Sequence != ev.Sequence ||
		back.Source.Confidence != ev.Source.Confidence || back.Corr == nil || back.Corr.CausationID != "evt_cause" {
		t.Fatalf("round trip lost fields: %+v", back)
	}
}

func TestSequenceIsMonotonicAcrossWritersInOneSession(t *testing.T) {
	dir := t.TempDir()
	const writers, each = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() { // a separate Spool/Recorder per writer stands in for a separate hook process
			defer wg.Done()
			r := newRecorder(t, dir, "sess-seq")
			for i := 0; i < each; i++ {
				if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionHeartbeat, Channel: runtimeevents.ChannelHook, Tier: Inferred}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	evs, err := (&Spool{Dir: dir}).Read("sess-seq")
	if err != nil || len(evs) != writers*each {
		t.Fatalf("events = %d err=%v", len(evs), err)
	}
	for i, ev := range evs {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("event %d has sequence %d, want %d", i, ev.Sequence, i+1)
		}
	}
}

func TestRecordRefusesWhatCannotBeAValidEvent(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir, "sess-bad")
	ctx := context.Background()
	for name, o := range map[string]Observation{
		"no kind":       {Channel: runtimeevents.ChannelHook, Tier: Exact},
		"no channel":    {Kind: runtimeevents.KindSessionIdle, Tier: Exact},
		"bad tier":      {Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: "sure"},
		"bare string":   {Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact, Payload: "hello"},
		"half an actor": {Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact, ActorKind: "agent"},
	} {
		if _, err := r.Record(ctx, o); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if evs, _ := (&Spool{Dir: dir}).Read("sess-bad"); len(evs) != 0 {
		t.Fatalf("a refused observation was spooled: %+v", evs)
	}
	if _, err := (&Recorder{Sink: &Spool{Dir: dir}}).Record(ctx, Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err == nil {
		t.Fatal("recorder without a session id accepted")
	}
}

func TestSpoolRefusesUnsafeSessionNamesAndOversizeLines(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}
	for _, id := range []string{"../escape", "a/b", ".hidden", "", strings.Repeat("a", 200)} {
		r := newRecorder(t, dir, id)
		if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err == nil {
			t.Fatalf("session id %q accepted", id)
		}
		if _, err := s.Read(id); err == nil {
			t.Fatalf("read of session id %q accepted", id)
		}
	}
	r := newRecorder(t, dir, "sess-big")
	if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindAgentDelta, Channel: runtimeevents.ChannelHook, Tier: Exact,
		Payload: map[string]string{"text": strings.Repeat("x", maxEventBytes)}}); err == nil {
		t.Fatal("oversize event accepted")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, _ := e.Info(); info != nil && info.Mode().Perm() != 0o600 && !e.IsDir() {
			t.Fatalf("%s has mode %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}
}

func TestReadSkipsATornLine(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir, "sess-torn")
	for i := 0; i < 2; i++ {
		if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "sess-torn.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{\"schema_version\":\"1\",\"id\":\"evt_torn\n")
	_ = f.Close()
	evs, err := (&Spool{Dir: dir}).Read("sess-torn")
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %d err=%v", len(evs), err)
	}
}

func TestPruneByAgeThenSize(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}
	now := time.Now()
	for i, name := range []string{"old", "mid", "new"} {
		r := newRecorder(t, dir, name)
		if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err != nil {
			t.Fatal(err)
		}
		mod := now.Add(-time.Duration(3-i) * 48 * time.Hour) // 144h, 96h, 48h old
		if err := os.Chtimes(filepath.Join(dir, name+".jsonl"), mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.Prune(now, 100*time.Hour, 0); err != nil || n != 1 {
		t.Fatalf("age prune removed %d err=%v, want 1 (old)", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.seq")); !os.IsNotExist(err) {
		t.Fatalf("sequence sidecar of a pruned session remains: %v", err)
	}
	if n, err := s.Prune(now, 0, 1); err != nil || n != 2 {
		t.Fatalf("size prune removed %d err=%v, want the remaining 2", n, err)
	}
	if n, err := (&Spool{Dir: filepath.Join(dir, "missing")}).Prune(now, time.Hour, 1); err != nil || n != 0 {
		t.Fatalf("missing dir: %d %v", n, err)
	}
}

func TestTruncatedOrMissingSidecarNeverRestartsTheSequence(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir, "sess-seq2")
	ctx := context.Background()
	obs := Observation{Kind: runtimeevents.KindSessionHeartbeat, Channel: runtimeevents.ChannelHook, Tier: Inferred}
	for i := 0; i < 3; i++ {
		if _, err := r.Record(ctx, obs); err != nil {
			t.Fatal(err)
		}
	}
	seq := filepath.Join(dir, "sess-seq2.seq")
	want := uint64(3)
	for _, c := range []struct {
		name   string
		damage func()
	}{
		{"empty", func() { _ = os.WriteFile(seq, nil, 0o600) }},
		{"garbage", func() { _ = os.WriteFile(seq, []byte("x1\n"), 0o600) }},
		{"missing", func() { _ = os.Remove(seq) }},
	} {
		c.damage()
		ev, err := r.Record(ctx, obs)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want++
		if ev.Sequence != want {
			t.Fatalf("%s sidecar: next sequence %d, want %d", c.name, ev.Sequence, want)
		}
	}
	evs, _ := (&Spool{Dir: dir}).Read("sess-seq2")
	seen := map[uint64]bool{}
	for _, ev := range evs {
		if seen[ev.Sequence] {
			t.Fatalf("duplicate sequence %d", ev.Sequence)
		}
		seen[ev.Sequence] = true
	}
}

func TestAnExplicitSequenceMustAdvance(t *testing.T) {
	dir := t.TempDir()
	r := newRecorder(t, dir, "sess-explicit")
	if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err != nil {
		t.Fatal(err)
	}
	s := &Spool{Dir: dir}
	ev := runtimeevents.Event{SchemaVersion: "1", ID: "evt_x", Kind: runtimeevents.KindSessionIdle, SessionID: "sess-explicit", Sequence: 1}
	if _, err := s.Append(context.Background(), ev); err == nil {
		t.Fatal("a sequence that does not advance was written")
	}
	ev.Sequence = 9
	if got, err := s.Append(context.Background(), ev); err != nil || got.Sequence != 9 {
		t.Fatalf("a later explicit sequence: %+v %v", got, err)
	}
}

func TestPruneSkipsASessionAWriterHolds(t *testing.T) {
	dir := t.TempDir()
	s := &Spool{Dir: dir}
	r := newRecorder(t, dir, "busy")
	if _, err := r.Record(context.Background(), Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "busy.jsonl"), old, old)
	f, err := os.OpenFile(filepath.Join(dir, "busy.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(f) // an Append in flight
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Prune(time.Now(), time.Hour, 0); err != nil || n != 0 {
		t.Fatalf("pruned a locked session: %d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "busy.jsonl")); err != nil {
		t.Fatalf("the locked file is gone: %v", err)
	}
	unlock()
	_ = f.Close()
	if n, err := s.Prune(time.Now(), time.Hour, 0); err != nil || n != 1 {
		t.Fatalf("after the writer finished: %d %v", n, err)
	}
}

func TestSpoolBoundsItselfDroppingTheOldestAndCountingThem(t *testing.T) {
	dir := t.TempDir()
	sp := &Spool{Dir: dir, MaxSessionBytes: 6000}
	r := &Recorder{App: "tether", SessionID: "sess-cap", Sink: sp, Process: runtimeevents.Process{Provider: "claude"}}
	ctx := context.Background()
	const total = 80
	for i := 0; i < total; i++ {
		if _, err := r.Record(ctx, Observation{Kind: runtimeevents.KindAgentToolUse, Channel: runtimeevents.ChannelHook, Tier: Exact,
			Payload: map[string]any{"tool": "Read", "i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "sess-cap.jsonl"))
	if err != nil || info.Size() > 6000 {
		t.Fatalf("spool file is %v bytes (err %v), over its 6000 byte cap", info.Size(), err)
	}
	evs, err := sp.Read("sess-cap")
	if err != nil || len(evs) == 0 || len(evs) >= total {
		t.Fatalf("kept %d of %d (err %v)", len(evs), total, err)
	}
	dropped := sp.Dropped("sess-cap")
	if dropped == 0 || int(dropped)+len(evs) != total {
		t.Fatalf("dropped %d + kept %d != %d", dropped, len(evs), total)
	}
	// The newest are kept, contiguous, and the sequence kept counting through the drops.
	if evs[len(evs)-1].Sequence != total {
		t.Fatalf("newest sequence %d, want %d", evs[len(evs)-1].Sequence, total)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Sequence != evs[i-1].Sequence+1 {
			t.Fatalf("kept events are not contiguous at %d: %d then %d", i, evs[i-1].Sequence, evs[i].Sequence)
		}
	}
	next, err := r.Record(ctx, Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact})
	if err != nil || next.Sequence != total+1 {
		t.Fatalf("after the drops: %+v %v", next, err)
	}
}

func TestSpoolDirectoryIsBoundedWhenASessionStarts(t *testing.T) {
	dir := t.TempDir()
	sp := &Spool{Dir: dir, MaxTotalBytes: 1}
	ctx := context.Background()
	for i, name := range []string{"first", "second", "third"} {
		r := &Recorder{App: "tether", SessionID: name, Sink: sp}
		if _, err := r.Record(ctx, Observation{Kind: runtimeevents.KindSessionIdle, Channel: runtimeevents.ChannelHook, Tier: Exact}); err != nil {
			t.Fatal(err)
		}
		// make the earlier ones older so "oldest first" is unambiguous
		old := time.Now().Add(-time.Duration(10-i) * time.Hour)
		_ = os.Chtimes(filepath.Join(dir, name+".jsonl"), old, old)
	}
	// Each new session's creation pruned the older ones past the 1 byte total; the newest session is never pruned by its own
	// creation (it holds its lock), so at most it remains.
	entries, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if len(entries) != 1 || !strings.HasSuffix(entries[0], "third.jsonl") {
		t.Fatalf("remaining spool files: %v", entries)
	}
}

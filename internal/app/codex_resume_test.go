package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// A stand-in `codex exec` that behaves the way the real one does where it matters
// here (CW-20261001-0210). Every call records its argv and CODEX_HOME. A thread
// belongs to the CODEX_HOME it started in, as in the real CLI: `exec ... resume
// <id>` finds it only if $CODEX_HOME/sessions/<id> exists, and otherwise exits 1
// with empty stdout and the stderr line real codex-cli 0.159.3 prints
// (confirmed against the real binary; see codexNoRolloutText).
const standInCodex = `#!/bin/sh
dir="$STANDIN_DIR"
n=$(ls "$dir"/argv.* 2>/dev/null | wc -l)
n=$((n+1))
printf '%s\n' "$@" > "$dir/argv.$n"
printf '%s' "$CODEX_HOME" > "$dir/home.$n"
id=""
prev=""
for a in "$@"; do
  if [ "$prev" = resume ]; then id="$a"; break; fi
  prev="$a"
done
if [ -n "$id" ]; then
  if [ ! -f "$CODEX_HOME/sessions/$id" ]; then
    echo "Error: thread/resume: thread/resume failed: no rollout found for thread id $id (code -32600)" >&2
    : > "$dir/done.$n"
    exit 1
  fi
  tid="$id"
else
  tid="$(printf '%08x-0000-4000-8000-%012x' "$$" "$n")"
  mkdir -p "$CODEX_HOME/sessions"
  : > "$CODEX_HOME/sessions/$tid"
fi
echo "{\"type\":\"thread.started\",\"thread_id\":\"$tid\"}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
: > "$dir/done.$n"
`

// resumeBus records the events the service publishes.
type resumeBus struct {
	mu  sync.Mutex
	got []events.Event
}

func (b *resumeBus) Publish(_ context.Context, e events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, e)
	return nil
}

func (b *resumeBus) Subscribe(context.Context, events.Filter) (<-chan events.Event, func(), error) {
	return nil, func() {}, errors.New("not supported")
}

func (b *resumeBus) kinds() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.got))
	for _, e := range b.got {
		out = append(out, e.Kind)
	}
	return out
}

// codexRig is a Service whose codex-cli provider is the stand-in above, over a real
// state DB and a minimal in-memory catalog, so resumes go through the real
// ResumeLogicalAgent path.
type codexRig struct {
	t    *testing.T
	svc  *Service
	bus  *resumeBus
	dir  string // where the stand-in records its calls
	repo string
}

func newCodexRig(t *testing.T) *codexRig {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte(standInCodex), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}
	t.Setenv("STANDIN_DIR", dir)

	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := t.TempDir()
	prov := config.Provider{ID: "codex-cli", Provider: "codex", Adapter: "codex", RuntimeKind: config.RuntimeKindSubprocess, Command: fake}
	cat := &config.Catalog{
		Global:    config.Global{Version: "test"},
		Projects:  map[string]config.Project{"proj": {ID: "proj", RepoRoot: repo, Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}},
		Agents:    map[string]config.Agent{"agent": {ID: "agent"}},
		Providers: map[string]config.Provider{"codex-cli": prov},
		Launches:  map[string]config.Launch{"codex-launch": {ID: "codex-launch", Project: "proj", Agent: "agent", Provider: "codex-cli"}},
	}
	factory, err := runtimeFactoryForProvider(prov)
	if err != nil {
		t.Fatalf("runtime factory: %v", err)
	}
	// app.New seeds a logical-agent row for every catalog agent; the rig skips
	// app.New, so it does the same.
	if _, err := seedLogicalAgents(db, cat.Agents); err != nil {
		t.Fatalf("seed logical agents: %v", err)
	}
	bus := &resumeBus{}
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     cat,
		Store:       db,
		Bus:         bus,
		Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
		factories:   map[string]RuntimeFactory{"codex-cli": factory},
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = svc.Manager.Shutdown(ctx)
	})
	return &codexRig{t: t, svc: svc, bus: bus, dir: dir, repo: repo}
}

// call is one recorded invocation of the stand-in.
type call struct {
	argv []string
	home string
}

// resumeID returns the id after `resume`, or "" when the call did not resume.
func (c call) resumeID() string {
	if i := slices.Index(c.argv, "resume"); i >= 0 && i+1 < len(c.argv) {
		return c.argv[i+1]
	}
	return ""
}

// wait returns the nth (1-based) call once it has finished.
func (r *codexRig) wait(n int) call {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(r.dir, fmt.Sprintf("done.%d", n))); err == nil {
			break
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("call %d of the stand-in never finished (calls so far: %d)", n, r.count())
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(filepath.Join(r.dir, fmt.Sprintf("argv.%d", n)))
	if err != nil {
		r.t.Fatal(err)
	}
	h, _ := os.ReadFile(filepath.Join(r.dir, fmt.Sprintf("home.%d", n)))
	args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if boundary := slices.Index(args, "--"); boundary >= 0 && boundary+1 < len(args) {
		args = append(args[:boundary+1], strings.Join(args[boundary+1:], "\n"))
	}
	return call{argv: args, home: string(h)}
}

func (r *codexRig) count() int {
	m, _ := filepath.Glob(filepath.Join(r.dir, "argv.*"))
	return len(m)
}

// start creates and launches a session of the codex launch.
func (r *codexRig) start() string {
	r.t.Helper()
	created, err := r.svc.CreateSession("codex-launch")
	if err != nil {
		r.t.Fatalf("CreateSession: %v", err)
	}
	if _, err := r.svc.LaunchSession(created.SessionID); err != nil {
		r.t.Fatalf("LaunchSession: %v", err)
	}
	r.t.Cleanup(func() { _ = r.svc.Manager.Stop(context.Background(), created.SessionID) })
	return created.SessionID
}

func (r *codexRig) turn(id, text string) error {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.svc.SendTurn(ctx, id, text)
}

func (r *codexRig) checkpoint(id, sourceSession, hints string) {
	r.t.Helper()
	if err := r.svc.Store.CreateCheckpoint(checkpoint.Checkpoint{
		ID: id, LogicalAgentID: "agent", Status: "in_progress", Summary: "work so far",
		SourceSessionID: sourceSession, ProviderHintsJSON: hints, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		r.t.Fatalf("CreateCheckpoint: %v", err)
	}
}

// threadOf returns the one thread the stand-in started in a CODEX_HOME.
func threadOf(t *testing.T, home string) string {
	t.Helper()
	threads, _ := filepath.Glob(filepath.Join(home, "sessions", "*"))
	if len(threads) != 1 {
		t.Fatalf("threads in %s: %v; want exactly one", home, threads)
	}
	return filepath.Base(threads[0])
}

// promptLast checks that the turn's prompt comes after "--", last, so nothing
// the convention puts before it (--cd, resume <id>) is read as prompt text.
func promptLast(t *testing.T, argv []string, prompt string) {
	t.Helper()
	dd := slices.Index(argv, "--")
	if dd < 0 || dd != len(argv)-2 || argv[len(argv)-1] != prompt {
		t.Fatalf("want ... -- %q at the end of argv: %q", prompt, argv)
	}
}

func onceBefore(t *testing.T, argv []string, flag string, limit int) {
	t.Helper()
	n, at := 0, -1
	for i, a := range argv {
		if a == flag {
			n++
			at = i
		}
	}
	if n != 1 || at >= limit {
		t.Fatalf("%s appears %d times (last at %d), want once before index %d: %q", flag, n, at, limit, argv)
	}
}

// Within one session (one boot dir, one CODEX_HOME) turn 2 resumes the thread
// turn 1 started: `exec ... --cd <project> resume <id> -- <prompt>`, with every
// exec option in front of the subcommand, where codex-cli accepts it
// (go-providers v0.41, agentkit v0.20.4 and v0.21.0). Before the bump each turn
// started a new thread. CW-20261001-0210.
func TestCodexExec_SecondTurnResumesTheThreadWithinOneSession(t *testing.T) {
	r := newCodexRig(t)
	a := r.start()

	if err := r.turn(a, "first turn"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	c1 := r.wait(1)
	if c1.resumeID() != "" {
		t.Fatalf("turn 1 resumes a thread that does not exist yet: %q", c1.argv)
	}
	promptLast(t, c1.argv, "first turn")
	thread := threadOf(t, c1.home)

	if err := r.turn(a, "second turn"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	c2 := r.wait(2)
	if c2.home != c1.home {
		t.Fatalf("turn 2 ran in another CODEX_HOME: %s vs %s", c2.home, c1.home)
	}
	if got := c2.resumeID(); got != thread {
		t.Fatalf("turn 2 resumes %q, want the thread turn 1 started, %q: %q", got, thread, c2.argv)
	}
	resume := slices.Index(c2.argv, "resume")
	// --cd (codex takes it only before the subcommand), the posture's -c overrides
	// and --json all sit in front of `resume`; each appears once.
	for _, flag := range []string{"--cd", "--json", "--skip-git-repo-check"} {
		onceBefore(t, c2.argv, flag, resume)
	}
	if cd := slices.Index(c2.argv, "--cd"); c2.argv[cd+1] != r.repo {
		t.Fatalf("--cd %q, want the project %q: %q", c2.argv[cd+1], r.repo, c2.argv)
	}
	if n := countToken(c2.argv, "-c"); n != 2 {
		t.Fatalf("-c appears %d times, want the posture's two overrides: %q", n, c2.argv)
	}
	for i, a := range c2.argv {
		if a == "-c" && i > resume {
			t.Fatalf("a -c override sits after resume, where codex refuses it: %q", c2.argv)
		}
	}
	promptLast(t, c2.argv, "second turn")
}

// Across canonical sessions, recovery preserves the provider conversation while
// fresh planting still supplies current config/auth/loopback in a distinct home.
func TestCodexExec_LogicalAgentResumeContinuesNativeThreadAcrossBoots(t *testing.T) {
	r := newCodexRig(t)
	a := r.start()
	if err := r.turn(a, "first turn"); err != nil {
		t.Fatal(err)
	}
	c1 := r.wait(1)
	thread := threadOf(t, c1.home)
	if err := r.svc.StopSession(a); err != nil {
		t.Fatal(err)
	}
	r.ended(a)
	// Native mapping is sufficient; no checkpoint is required.
	res, err := r.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID == a {
		t.Fatal("ordinary resume reused canonical session")
	}
	c2 := r.wait(2)
	if c2.home == c1.home || c2.resumeID() != thread {
		t.Fatalf("native recovery argv=%v home=%s", c2.argv, c2.home)
	}
	if !strings.Contains(c2.argv[len(c2.argv)-1], "Recovery pack v0") {
		t.Fatalf("missing recovery context: %q", c2.argv)
	}
	r.idle(res.SessionID)
	if err = r.turn(res.SessionID, "after recovery"); err != nil {
		t.Fatal(err)
	}
	c3 := r.wait(3)
	if c3.resumeID() != thread || c3.home != c2.home {
		t.Fatalf("followup lost native identity: %+v", c3)
	}
	promptLast(t, c3.argv, "after recovery")
}

func TestCodexExec_MissingNativeStateColdBootsOnceWithRecoveryPack(t *testing.T) {
	r := newCodexRig(t)
	a := r.start()
	if err := r.turn(a, "first turn"); err != nil {
		t.Fatal(err)
	}
	c1 := r.wait(1)
	thread := threadOf(t, c1.home)
	if err := r.svc.StopSession(a); err != nil {
		t.Fatal(err)
	}
	r.ended(a)
	if err := os.Remove(filepath.Join(c1.home, "sessions", thread)); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failed := r.wait(2)
	cold := r.wait(3)
	if failed.resumeID() != thread || cold.resumeID() != "" || cold.home != failed.home {
		t.Fatalf("native/cold attempts: %+v %+v", failed, cold)
	}
	if !strings.Contains(cold.argv[len(cold.argv)-1], "Recovery pack v0") {
		t.Fatalf("cold boot omitted recovery pack: %q", cold.argv)
	}
	if !slices.Contains(r.bus.kinds(), events.KindProviderSessionLost) {
		t.Fatal("missing native-loss continuity event")
	}
	source, err := r.svc.Store.GetSessionProviderMapping(a, "tether", "codex-cli")
	if err != nil || source.NativeSessionID.Valid {
		t.Fatalf("stale source mapping: %+v %v", source, err)
	}
	plan, err := r.svc.Store.GetLaunchPlan(res.SessionID)
	if err != nil || plan.ResumeProviderSessionID != "" {
		t.Fatalf("stale persisted hint: %+v %v", plan, err)
	}
	r.idle(res.SessionID)
	if err = r.turn(res.SessionID, "continue fresh conversation"); err != nil {
		t.Fatal(err)
	}
	next := r.wait(4)
	if next.resumeID() == "" || next.resumeID() == thread {
		t.Fatalf("followup=%+v", next)
	}
}

func (r *codexRig) idle(id string) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h, ok := r.svc.Manager.Health(id); ok && h.Health.Alive && h.Health.State == agentsessions.LiveStateIdle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("recovery control turn did not become idle")
}

func (r *codexRig) ended(id string) {
	r.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		row, err := r.svc.Store.GetSession(id)
		if err == nil && (row.State == "killed" || row.State == "failed" || row.State == "completed") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("stopped runtime did not record terminal state")
}

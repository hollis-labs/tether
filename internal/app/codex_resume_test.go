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
	"github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

var providerSessionLost = provider.ErrProviderSessionLost

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

// codexNoRolloutText is the stderr line real codex-cli 0.159.3 prints when asked
// to resume a thread its CODEX_HOME lacks (empty stdout, exit 1).
const codexNoRolloutText = "no rollout found for thread id"

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
	return call{argv: strings.Split(strings.TrimSpace(string(b)), "\n"), home: string(h)}
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

// A logical-agent resume is a NEW session with a NEW boot dir, so a new
// CODEX_HOME. With a checkpoint that records no provider thread id (which is every
// checkpoint Tether itself writes today) it starts a fresh codex thread: its first
// turn has no `resume`. Within that new session, turn 2 resumes the new session's
// own thread. This pins what happens today across boots (CW-20261001-0210).
func TestCodexExec_LogicalAgentResumeStartsAFreshThreadAcrossBoots(t *testing.T) {
	r := newCodexRig(t)
	a := r.start()
	if err := r.turn(a, "first turn"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	c1 := r.wait(1)
	threadA := threadOf(t, c1.home)
	if err := r.svc.StopSession(a); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	r.checkpoint("ck1", a, "")

	res, err := r.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
	if err != nil {
		t.Fatalf("ResumeLogicalAgent: %v", err)
	}
	if res.SessionID == a {
		t.Fatal("a resume must be a new session")
	}
	if err := r.turn(res.SessionID, "after resume"); err != nil {
		t.Fatalf("first turn after the resume: %v", err)
	}
	c2 := r.wait(2)
	if c2.home == c1.home {
		t.Fatalf("the resumed session reused the old CODEX_HOME %s", c1.home)
	}
	if c2.resumeID() != "" {
		t.Fatalf("the resumed session's first turn resumes %q; nothing carries a thread id across boots: %q", c2.resumeID(), c2.argv)
	}
	promptLast(t, c2.argv, "after resume")
	threadB := threadOf(t, c2.home)
	if threadB == threadA {
		t.Fatal("the resumed session continued the old thread")
	}

	if err := r.turn(res.SessionID, "and again"); err != nil {
		t.Fatalf("second turn after the resume: %v", err)
	}
	c3 := r.wait(3)
	if got := c3.resumeID(); got != threadB || c3.home != c2.home {
		t.Fatalf("within the resumed session turn 2 should resume ITS thread %q in %s, got %q in %s: %q", threadB, c2.home, got, c3.home, c3.argv)
	}
}

// The one way a thread id can reach a new boot: a checkpoint whose provider hints
// carry it. Nothing in Tether writes those hints today, but if something starts
// to, a resume into a CODEX_HOME that lacks the thread must not become a hard
// failure. Real codex-cli 0.159.3 exits 1 with empty stdout and "Error:
// thread/resume: thread/resume failed: no rollout found for thread id <id> (code
// -32600)". The lib classifies that as a lost session: the turn fails ONCE with a
// typed ErrProviderSessionLost (which the API answers as 409 provider_session_lost,
// not a 502 turn_failed), the session log says so, the id is dropped, and the next
// turn starts a fresh thread. The resume itself, the launch, succeeds.
func TestCodexExec_ResumeWithAThreadFromAnotherBootIsASessionLossNotAHardFailure(t *testing.T) {
	r := newCodexRig(t)
	a := r.start()
	if err := r.turn(a, "first turn"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	c1 := r.wait(1)
	threadA := threadOf(t, c1.home)
	if err := r.svc.StopSession(a); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	r.checkpoint("ck1", a, fmt.Sprintf(`{"provider_session_id":%q}`, threadA))

	res, err := r.svc.ResumeLogicalAgent("agent", api.ResumeOptions{})
	if err != nil {
		t.Fatalf("the resume itself must not fail: %v", err)
	}

	// Turn 1 of the new boot asks codex to resume a thread its CODEX_HOME lacks.
	lossErr := r.turn(res.SessionID, "after resume (1)")
	c2 := r.wait(2)
	if c2.home == c1.home {
		t.Fatal("the resumed session reused the old CODEX_HOME, so this tests nothing")
	}
	if got := c2.resumeID(); got != threadA {
		t.Fatalf("the checkpoint's thread %q was not fed to the new boot's first turn (got %q): %q", threadA, got, c2.argv)
	}
	if lossErr == nil {
		t.Fatal("resuming a thread the CODEX_HOME lacks should fail that turn once")
	}
	if !errors.Is(lossErr, providerSessionLost) {
		t.Fatalf("the failure must be a typed provider session loss (the API's 409 provider_session_lost), got: %v", lossErr)
	}
	if !strings.Contains(lossErr.Error(), codexNoRolloutText) {
		t.Fatalf("the error should carry codex's own reason (%q): %v", codexNoRolloutText, lossErr)
	}
	// The session log records the loss, so an operator reading it can see why.
	row, err := r.svc.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	logText, err := os.ReadFile(filepath.Join(row.Workspace, "logs", "session.log"))
	if err != nil {
		t.Fatalf("session log: %v", err)
	}
	if !strings.Contains(string(logText), "[session_lost] requested="+threadA) {
		t.Fatalf("the session log does not record the loss:\n%s", logText)
	}

	// The id was dropped: the next turn starts a fresh thread and succeeds.
	if err := r.turn(res.SessionID, "after resume (2)"); err != nil {
		t.Fatalf("the turn after a session loss must succeed on a fresh thread: %v", err)
	}
	c3 := r.wait(3)
	if c3.resumeID() != "" {
		t.Fatalf("the turn after a loss still resumes %q: %q", c3.resumeID(), c3.argv)
	}
	promptLast(t, c3.argv, "after resume (2)")
	if threadB := threadOf(t, c3.home); threadB == threadA {
		t.Fatal("the fresh thread has the lost thread's id")
	}
	// Not asserted, on purpose: whether a provider.session_lost event is published
	// here. agentkit calls OnProviderSessionLost only when a turn ran on in a NEW
	// session (Antigravity's case), so for this failed turn the lib emits its own
	// typed event and the session-log marker, and Tether publishes nothing on the
	// bus. That is a decision for the lead; see the PR.
	t.Logf("bus events (informational): %v", r.bus.kinds())
}

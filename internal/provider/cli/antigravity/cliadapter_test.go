package antigravity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/agentkit/agentsessions/compliance"
	llmtypes "github.com/hollis-labs/go-llm-types"
	gop "github.com/hollis-labs/go-providers/provider"
	gopevents "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// isolateHome points HOME at an empty temp dir, so nothing under test can
// reach the real ~/.gemini.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// TestCompliance runs the shared agentsessions compliance suite with an
// sh-backed plan, so no real agy is needed.
func TestCompliance(t *testing.T) {
	isolateHome(t)
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := New(&launch.Plan{Command: shPath, Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	compliance.Run(t, compliance.Harness{
		Runtime: rt,
		NewStartOptions: func(t *testing.T) agentsessions.StartOptions {
			dir := t.TempDir()
			return agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log")}
		},
		BinarySkip: true,
	})
}

// fakeAgy stands in for `agy --output-format stream-json [...] -p=<prompt>`.
// It logs its argv, keeps a --conversation id it issued, and answers any
// other id the way agy 1.2.7 does: a stderr warning, exit 0 and a new
// conversation. Its turn is agy's shape: init, a text step with usage, a
// result with one auto-denied action.
func fakeAgy(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test script needs sh")
	}
	path := filepath.Join(dir, "agy")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + filepath.Join(dir, "argv.log") + `"
conv=conv-fresh
while [ $# -gt 0 ]; do
  if [ "$1" = "--conversation" ]; then conv=$2; fi
  shift
done
case "$conv" in
  conv-fresh) ;;
  *) printf 'warning: conversation "%s" not found\n' "$conv" 1>&2; conv=conv-replacement ;;
esac
printf '{"event":"init","conversation_id":"%s","init":{"cwd":"/w","tools":[],"permission_mode":"request-review"}}\n' "$conv"
printf '{"event":"step_update","step_update":{"conversation_id":"%s","step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"hi","usage":{"input_tokens":3,"output_tokens":5,"thinking_tokens":1,"cache_read_tokens":0,"total_tokens":8}}}\n' "$conv"
printf '{"event":"result","result":{"conversation_id":"%s","status":"SUCCESS","response":"hi","num_turns":1,"denied_actions":[{"action":"command","display_name":"RunCommand"}]}}\n' "$conv"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func argvLog(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

type harness struct {
	sess   agentsessions.Session
	events chan llmtypes.StreamEvent
	typed  *[]gopevents.Event
	lost   *[][3]string
	dir    string
}

func start(t *testing.T, mode, preset string) harness {
	t.Helper()
	isolateHome(t)
	dir := t.TempDir()
	rt, err := New(&launch.Plan{Command: fakeAgy(t, dir), PermissionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h := harness{events: make(chan llmtypes.StreamEvent, 64), typed: &[]gopevents.Event{}, lost: &[][3]string{}, dir: dir}
	h.sess, err = rt.Start(context.Background(), agentsessions.StartOptions{
		Workdir:            dir,
		LogPath:            filepath.Join(dir, "session.log"),
		SessionIDPreset:    preset,
		EventFanout:        h.events,
		TypedEventCallback: func(ev gopevents.Event) { *h.typed = append(*h.typed, ev) },
		OnProviderSessionLost: func(r, a, reason string) {
			*h.lost = append(*h.lost, [3]string{r, a, reason})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.sess.Stop(context.Background()) })
	return h
}

func drain(ch chan llmtypes.StreamEvent) []llmtypes.EventType {
	var out []llmtypes.EventType
	for {
		select {
		case ev := <-ch:
			out = append(out, ev.Type)
		default:
			return out
		}
	}
}

func TestRuntime_ArgvEventsAndResume(t *testing.T) {
	h := start(t, config.PermissionModeDefault, "")
	for _, p := range []string{"one", "two"} {
		if err := h.sess.SendInput(context.Background(), []byte(p)); err != nil {
			t.Fatalf("SendInput(%s): %v", p, err)
		}
	}
	want := []string{
		"--output-format stream-json --mode accept-edits -p=one",
		"--output-format stream-json --mode accept-edits --conversation conv-fresh -p=two",
	}
	if got := argvLog(t, h.dir); !slices.Equal(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
	turn := []llmtypes.EventType{llmtypes.EventSessionID, llmtypes.EventDelta, llmtypes.EventUsage, llmtypes.EventDone}
	if got := drain(h.events); !slices.Equal(got, append(slices.Clone(turn), turn...)) {
		t.Errorf("events = %v; want %v twice", got, turn)
	}
	var denied int
	for _, ev := range *h.typed {
		if _, ok := ev.(gopevents.PermissionDenied); ok {
			denied++
		}
	}
	if denied != 2 || len(*h.lost) != 0 {
		t.Errorf("permission denials = %d (want one per turn), lost = %v", denied, *h.lost)
	}
}

func TestRuntime_BypassPosture(t *testing.T) {
	h := start(t, config.PermissionModeBypass, "")
	if err := h.sess.SendInput(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := argvLog(t, h.dir); len(got) != 1 || !strings.Contains(got[0], "--dangerously-skip-permissions") || strings.Contains(got[0], "--mode") {
		t.Errorf("argv = %q", got)
	}
}

// An unknown conversation id: the turn succeeds in a new conversation, and
// the replacement is reported once, through PlanScopedAdapter's forward of
// the adapter's SessionResumeVerifier.
func TestRuntime_ReplacedConversationIsReported(t *testing.T) {
	h := start(t, config.PermissionModeDefault, "conv-stale")
	if err := h.sess.SendInput(context.Background(), []byte("x")); err != nil {
		t.Fatalf("turn with a stale id must still succeed: %v", err)
	}
	if len(*h.lost) != 1 || (*h.lost)[0][0] != "conv-stale" || (*h.lost)[0][1] != "conv-replacement" {
		t.Fatalf("lost = %v", *h.lost)
	}
	if got := h.sess.(agentsessions.SessionIDer).ProviderSessionID(); got != "conv-replacement" {
		t.Errorf("stored id = %q", got)
	}
}

// Gemini CLI's ~/.gemini/oauth_creds.json is not agy's credential (agy's
// is a Keychain token), so its absence must not refuse an agy launch. The
// adapter's own Preflight still stats it (CW-20260930-0221 R1); New does not
// forward that check.
func TestRuntime_PrepareIgnoresMissingGeminiCLICredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := os.Stat(filepath.Join(home, ".gemini", "oauth_creds.json")); !os.IsNotExist(err) {
		t.Fatalf("temp HOME must lack oauth_creds.json: %v", err)
	}
	// The adapter's check on its own still refuses here, so the pass below
	// is New's doing and not an empty check.
	if err := gop.NewAntigravityAdapter().Preflight(); !errors.Is(err, gop.ErrProviderNotAuthenticated) {
		t.Fatalf("adapter Preflight = %v; want ErrProviderNotAuthenticated", err)
	}
	rt, err := New(&launch.Plan{Command: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare = %v; want nil without oauth_creds.json", err)
	}
}

// Dropping the Preflight forward must keep the auth-failure classifier: a
// turn that ends in agy's auth failure still reports not-authenticated.
func TestRuntime_AuthFailureStillClassified(t *testing.T) {
	isolateHome(t)
	if runtime.GOOS == "windows" {
		t.Skip("test script needs sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	body := "#!/bin/sh\necho 'Error: authentication failed or timed out' 1>&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	rt, err := New(&launch.Plan{Command: bin})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	sess, err := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	if err := sess.SendInput(context.Background(), []byte("x")); !errors.Is(err, gop.ErrProviderNotAuthenticated) {
		t.Fatalf("SendInput = %v; want ErrProviderNotAuthenticated", err)
	}
}

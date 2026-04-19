package claudecode

import (
	"context"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
)

// TestAdapter_Prepare_EmptyCommandErrors proves validation surfaces before
// any workspace state is touched.
func TestAdapter_Prepare_EmptyCommandErrors(t *testing.T) {
	err := Adapter{}.Prepare(context.Background(), &launch.Plan{})
	if err == nil {
		t.Fatal("expected empty-command error")
	}
}

// TestAdapter_Start_RunsToCompletion exercises the full Runtime.Start →
// Session flow against /bin/true. It is the end-to-end gate that the env
// composition, PTY start, and Session wrapper all wire up correctly.
func TestAdapter_Start_RunsToCompletion(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("PTY path requires unix")
	}
	if _, err := exec.LookPath("/bin/true"); err != nil {
		t.Skipf("/bin/true missing: %v", err)
	}

	tmp := t.TempDir()
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("MUXTEST_INHERIT", "1")

	plan := &launch.Plan{
		Command: "/bin/true",
		EnvMode: "merge",
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir: tmp,
		LogPath: filepath.Join(tmp, "session.log"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan int, 1)
	go func() {
		code, _ := sess.Wait()
		done <- code
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		_ = sess.Stop(context.Background())
		t.Fatal("Wait did not return within 5s")
	}

	if h := sess.Health(); h.PID == 0 {
		t.Error("Health.PID = 0, expected a real PID after Start")
	}
}

// TestAdapter_Start_PropagatesEnvErrorFromPrepare guards the narrow path
// where Start-without-Prepare still validates. (Manager's contract is to
// call Prepare separately; this belt-and-suspenders check keeps Start from
// silently launching a broken plan.)
func TestAdapter_Start_EmptyCommandErrors(t *testing.T) {
	_, err := Adapter{}.Start(context.Background(), &launch.Plan{}, provider.StartOptions{})
	if err == nil {
		t.Fatal("expected Start to reject empty command")
	}
}

// TestAdapter_StaticInterfaceConformance is redundant with the var _ check
// in adapter.go but keeps a loud test-time failure if a future refactor
// silently breaks the contract.
func TestAdapter_StaticInterfaceConformance(t *testing.T) {
	var _ provider.Runtime = Adapter{}
	var _ provider.Session = (*cliSession)(nil)
}

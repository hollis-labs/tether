package mcpadapter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hollis-labs/go-mcp/supervise"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// stdioUpstream owns Wait; the MCP transport only owns the protocol pipes.
// A replacement is never started until Wait confirms the old process exited.
// Closing a connection sends EOF, never a signal to a live upstream.
type stdioUpstream struct {
	*mcpsdk.ClientSession
	cmd    *exec.Cmd
	done   chan struct{}
	lost   chan struct{}
	stdin  io.WriteCloser
	stdout *os.File
	stderr supervise.Tail
	exit   supervise.Exit // published by closing done
	launch LaunchObservation
}

// eofReader must satisfy io.ReadCloser, not just io.Reader: mcpsdk.IOTransport
// requires a ReadCloser, so Close is promoted from the embedded stdout handle
// rather than added by hand here.
type eofReader struct {
	io.ReadCloser
	once sync.Once
	lost chan struct{}
}

func (r *eofReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	if err != nil {
		r.once.Do(func() { close(r.lost) })
	}
	return n, err
}

// spawnStdioUpstream starts the upstream process and wires its pipes for the
// process-supervision observability client_pool.go's recovery loop depends on
// (separate lost-vs-exited signals, bounded redacted stderr tail, exit
// code/signal capture), returning a *stdioUpstream with no ClientSession yet
// and the mcpsdk.Transport to connect it over.
//
// The MCP client itself is deliberately NOT constructed here: it needs a
// RelaunchObservation built from u.launch (see ClientPool.connect), which is
// only known once the process has actually started -- and go-mcp's
// ClientOptions.Capabilities can only be supplied at Client construction,
// before Connect. Splitting spawn from connect lets the caller build that
// payload from the now-known u.launch and hand it to mcpsdk.NewClient itself.
//
// This also means: NOT mcpsdk.CommandTransport, which owns Cmd construction
// itself and exposes no hook to intercept reads for "lost" detection or to
// attach our own Stderr/WaitDelay configuration.
func spawnStdioUpstream(entry config.MCPServerEntry) (*stdioUpstream, *mcpsdk.IOTransport, error) {
	if entry.Command == "" {
		return nil, nil, fmt.Errorf("stdio transport requires command")
	}
	// #nosec G204 -- Executing the user's configured MCP command is the stdio transport contract; no shell is involved.
	u := &stdioUpstream{cmd: exec.Command(entry.Command, entry.Args...), done: make(chan struct{}), lost: make(chan struct{})}
	u.launch = observeLaunch(u.cmd, entry)
	u.cmd.Env = os.Environ()
	u.stderr.Secrets = stderrRedactionValues(entry)
	for k, v := range entry.Env {
		u.cmd.Env = append(u.cmd.Env, k+"="+v)
	}
	u.cmd.Stderr = &u.stderr
	// Bound waiting for inherited stderr pipes after the process itself exits.
	u.cmd.WaitDelay = time.Second
	stdin, err := u.cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	u.stdout = stdout
	u.stdin = stdin
	u.cmd.Stdout = writer
	if err := u.cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = writer.Close()
		return nil, nil, err
	}
	u.launch.PID = u.cmd.Process.Pid
	_ = writer.Close()
	transport := &mcpsdk.IOTransport{
		Reader: &eofReader{ReadCloser: stdout, lost: u.lost},
		Writer: stdin,
	}
	return u, transport, nil
}

// abandon is called when the handshake over an already-spawned process fails
// (timeout, garbage response, ...): no session was ever established, so
// there is nothing for watchExit's exit-tracking to race against or to
// release. It closes stdin (the transport's normal shutdown signal -- see
// pipeRWC.Close in the official SDK's CommandTransport) and reaps the
// process so a child that never notices EOF does not leak as a zombie.
func (u *stdioUpstream) abandon() {
	_ = u.stdin.Close()
	_ = u.stdout.Close()
	go func() { _ = u.cmd.Wait() }()
}

// watchExit starts the goroutine that observes process exit once a
// ClientSession has been established (u.ClientSession must already be set).
// It publishes an Exit and closes done for every exit, clean or not.
func (u *stdioUpstream) watchExit() {
	go func() {
		_ = u.cmd.Wait()
		u.exit = supervise.ClassifyExit(u.cmd.ProcessState, time.Now().UTC())
		_ = u.Close() // releases in-flight calls, even if a descendant holds stdout
		close(u.done)
	}()
}

func stderrRedactionValues(entry config.MCPServerEntry) []string {
	values := []string{entry.Token}
	values = append(values, entry.ArgumentRedactionValues()...)
	for _, value := range entry.Env {
		values = append(values, value)
	}
	return values
}

func (u *stdioUpstream) Close() error {
	err := u.ClientSession.Close()
	_ = u.stdout.Close()
	return err
}

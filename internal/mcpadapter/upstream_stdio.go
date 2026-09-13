package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"

	"github.com/hollis-labs/tether/internal/config"
)

// stdioUpstream owns Wait; the MCP transport only owns the protocol pipes.
// A replacement is never started until Wait confirms the old process exited.
// Closing a connection sends EOF, never a signal to a live upstream.
type stdioUpstream struct {
	*mcpclient.Client
	cmd    *exec.Cmd
	done   chan struct{}
	lost   chan struct{}
	stdout *os.File
	stderr stderrTail
	exit   UpstreamExit // published by closing done
}

type UpstreamExit struct {
	Kind   string    `json:"kind"` // clean, error, signal
	Code   int       `json:"code"`
	Signal string    `json:"signal,omitempty"`
	At     time.Time `json:"at"`
}

type eofReader struct {
	io.Reader
	once sync.Once
	lost chan struct{}
}

func (r *eofReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if errors.Is(err, os.ErrClosed) {
		err = io.EOF
	}
	if err != nil {
		r.once.Do(func() { close(r.lost) })
	}
	return n, err
}

const stderrTailBytes = 8192

// Drain continuously so an upstream cannot block on a full stderr pipe.
// Raw diagnostics stay bounded in this process; only explicit status reads
// expose the tail, with catalog-supplied values redacted.
type stderrTail struct {
	mu      sync.Mutex
	data    []byte
	secrets []string
}

func (b *stderrTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= stderrTailBytes {
		b.data = append(b.data[:0], p[len(p)-stderrTailBytes:]...)
	} else {
		b.data = append(b.data, p...)
		if len(b.data) > stderrTailBytes {
			b.data = append(b.data[:0], b.data[len(b.data)-stderrTailBytes:]...)
		}
	}
	return n, nil
}

func (b *stderrTail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := redactStderr(string(b.data), b.secrets)
	if len(s) > stderrTailBytes {
		s = s[len(s)-stderrTailBytes:]
	}
	return s
}

type redactionRange struct {
	start int
	end   int
}

// redactStderr identifies every match against the same immutable snapshot.
// Applying one configured value must not alter the bytes another value needs
// to match, because catalog environment map iteration has no stable order.
func redactStderr(original string, secrets []string) string {
	ranges := make([]redactionRange, 0, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for from := 0; from < len(original); {
			offset := strings.Index(original[from:], secret)
			if offset < 0 {
				break
			}
			start := from + offset
			ranges = append(ranges, redactionRange{start: start, end: start + len(secret)})
			from = start + 1 // retain overlapping matches
		}
		// The retained window may begin partway through a configured value.
		for n := min(len(secret)-1, len(original)); n > 0; n-- {
			if strings.HasPrefix(original, secret[len(secret)-n:]) {
				ranges = append(ranges, redactionRange{end: n})
				break
			}
		}
		// A snapshot may be read between arbitrary stderr writes. Cover the
		// longest unfinished value prefix at the live end before publishing it.
		for n := min(len(secret)-1, len(original)); n > 0; n-- {
			if strings.HasSuffix(original, secret[:n]) {
				ranges = append(ranges, redactionRange{start: len(original) - n, end: len(original)})
				break
			}
		}
	}
	if len(ranges) == 0 {
		return original
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].start != ranges[j].start {
			return ranges[i].start < ranges[j].start
		}
		return ranges[i].end < ranges[j].end
	})
	merged := ranges[:1]
	for _, next := range ranges[1:] {
		last := &merged[len(merged)-1]
		if next.start <= last.end {
			last.end = max(last.end, next.end)
			continue
		}
		merged = append(merged, next)
	}
	var out strings.Builder
	out.Grow(len(original))
	cursor := 0
	for _, match := range merged {
		out.WriteString(original[cursor:match.start])
		out.WriteString("[redacted]")
		cursor = match.end
	}
	out.WriteString(original[cursor:])
	return out.String()
}

func newStdioUpstream(ctx context.Context, entry config.MCPServerEntry) (*stdioUpstream, error) {
	if entry.Command == "" {
		return nil, fmt.Errorf("stdio transport requires command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// #nosec G204 -- Executing the user's configured MCP command is the stdio transport contract; no shell is involved.
	u := &stdioUpstream{cmd: exec.Command(entry.Command, entry.Args...), done: make(chan struct{}), lost: make(chan struct{})}
	u.cmd.Env = os.Environ()
	u.stderr.secrets = append(u.stderr.secrets, entry.Token)
	for k, v := range entry.Env {
		u.cmd.Env = append(u.cmd.Env, k+"="+v)
		u.stderr.secrets = append(u.stderr.secrets, v)
	}
	u.cmd.Stderr = &u.stderr
	// Bound waiting for inherited stderr pipes after the process itself exits.
	u.cmd.WaitDelay = time.Second
	stdin, err := u.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, writer, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	u.stdout = stdout
	u.cmd.Stdout = writer
	if err := u.cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = writer.Close()
		return nil, err
	}
	_ = writer.Close()
	t := transport.NewIO(&eofReader{Reader: stdout, lost: u.lost}, stdin, nil)
	u.Client = mcpclient.NewClient(t)
	// NewIO.Start does not spawn or dial and cannot fail.
	_ = t.Start(ctx)
	go func() {
		_ = u.cmd.Wait()
		u.exit = UpstreamExit{Kind: "error", Code: u.cmd.ProcessState.ExitCode(), At: time.Now().UTC()}
		if u.cmd.ProcessState.Success() {
			u.exit.Kind = "clean"
		}
		if status, ok := u.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			u.exit.Kind = "signal"
			u.exit.Signal = status.Signal().String()
		}
		_ = u.Close() // releases in-flight calls, even if a descendant holds stdout
		close(u.done)
	}()
	return u, nil
}

func (u *stdioUpstream) Close() error {
	err := u.Client.Close()
	_ = u.stdout.Close()
	return err
}

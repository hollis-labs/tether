// Package jsonltail follows an append-only JSONL file by polling and yields each complete line once. It is how the
// chat backends read files other processes write: Claude's transcript, Tether's hook spool.
package jsonltail

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
)

// DefaultPoll is how often a Tail checks the file when Poll is zero.
const DefaultPoll = 150 * time.Millisecond

// maxLine bounds one buffered line; a longer one is dropped whole (it cannot be a line this reader needs: transcript
// lines with large tool output are the only ones near it, and they are skipped, not misparsed).
const maxLine = 32 << 20

// Tail follows one file.
type Tail struct {
	// Path is the file. It may not exist yet; Tail waits for it.
	Path string
	// Poll is the check interval (DefaultPoll when zero).
	Poll time.Duration
	// FromEnd starts at the file's size when Run first opens it, so only lines appended later are yielded. Otherwise
	// Tail starts at the beginning.
	FromEnd bool
	// OnReset, when set, is called when the file shrank or was replaced: Tail starts it over from the beginning, and the
	// caller drops what it would see twice (the hook spool rewrites itself when it trims its oldest lines).
	OnReset func()
}

// Run yields complete lines (without the newline) to emit until ctx ends. A trailing line with no newline is held
// until it is finished. Run returns ctx's error, or the first read error that is not the file being absent.
func (t *Tail) Run(ctx context.Context, emit func(line []byte)) error {
	poll := t.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	var (
		off      int64
		started  bool
		ident    os.FileInfo
		partial  []byte
		skipping bool // inside an over-long line, discarding to its newline
	)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		fi, err := os.Stat(t.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Not written yet, or removed: wait.
		case err != nil:
			return err
		default:
			if !started {
				started = true
				if t.FromEnd {
					off = fi.Size()
				}
			} else if fi.Size() < off || (ident != nil && !os.SameFile(ident, fi)) {
				off, partial, skipping = 0, nil, false
				if t.OnReset != nil {
					t.OnReset()
				}
			}
			ident = fi
			if fi.Size() > off {
				n, buf, rerr := readFrom(t.Path, off)
				if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
					return rerr
				}
				off += n
				if skipping {
					i := bytes.IndexByte(buf, '\n')
					if i < 0 {
						buf = nil
					} else {
						buf, skipping = buf[i+1:], false
					}
				}
				partial = append(partial, buf...)
				for {
					i := bytes.IndexByte(partial, '\n')
					if i < 0 {
						break
					}
					line := bytes.TrimRight(partial[:i], "\r")
					if len(bytes.TrimSpace(line)) > 0 {
						emit(append([]byte(nil), line...))
					}
					partial = partial[i+1:]
				}
				if len(partial) > maxLine {
					partial, skipping = nil, true
				}
				partial = append([]byte(nil), partial...)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func readFrom(path string, off int64) (int64, []byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the caller names the file it follows
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, nil, err
	}
	b, err := io.ReadAll(f)
	return int64(len(b)), b, err
}

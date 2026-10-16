package agentevents

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// maxEventBytes bounds one spooled line: a single append of this size stays atomic for any reader that tails the file.
const maxEventBytes = 64 << 10

var sessionFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Spool is the local event store: one append-only JSONL file per session under Dir, plus a ".seq" sidecar holding the
// last sequence. Writers in different processes (each hook invocation is one) serialize on a file lock, so the sequence
// is monotonic per session across them. Dir and the files are private to the user (0700 / 0600).
type Spool struct {
	Dir string
}

// Append stores ev in its session's file and returns it with Sequence assigned when it was zero.
func (s *Spool) Append(_ context.Context, ev runtimeevents.Event) (runtimeevents.Event, error) {
	if s == nil || s.Dir == "" {
		return ev, errors.New("agentevents: spool has no directory")
	}
	if !sessionFileName.MatchString(ev.SessionID) {
		return ev, fmt.Errorf("agentevents: session id %q is not a safe file name", ev.SessionID)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return ev, fmt.Errorf("agentevents: spool dir: %w", err)
	}
	path := filepath.Join(s.Dir, ev.SessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: name checked above, dir is ours
	if err != nil {
		return ev, fmt.Errorf("agentevents: open spool: %w", err)
	}
	defer func() { _ = f.Close() }()
	unlock, err := lockFile(f)
	if err != nil {
		return ev, fmt.Errorf("agentevents: lock spool: %w", err)
	}
	defer unlock()
	seqPath := filepath.Join(s.Dir, ev.SessionID+".seq")
	last := lastSequence(seqPath, path)
	if ev.Sequence == 0 {
		ev.Sequence = last + 1
	} else if ev.Sequence <= last {
		return ev, fmt.Errorf("agentevents: sequence %d is not after the session's last (%d)", ev.Sequence, last)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return ev, fmt.Errorf("agentevents: marshal event: %w", err)
	}
	if len(line)+1 > maxEventBytes {
		return ev, fmt.Errorf("agentevents: event %s is %d bytes, over the %d byte spool line limit", ev.ID, len(line)+1, maxEventBytes)
	}
	// Record the sequence before the line: a process killed between the two leaves a gap, never a duplicate. The sidecar is
	// replaced by rename so it is never seen empty or partial.
	if err := writeSeq(seqPath, ev.Sequence); err != nil {
		return ev, fmt.Errorf("agentevents: write sequence: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return ev, fmt.Errorf("agentevents: write spool: %w", err)
	}
	return ev, nil
}

// lastSequence is the highest sequence the session has used. The sidecar is the fast path; when it is missing or
// unreadable, or lower than the file it describes (it was lost, or the file was written by an older writer), the spool file
// itself is the truth, so a damaged sidecar can never restart the sequence.
func lastSequence(seqPath, spoolPath string) uint64 {
	var last uint64
	if raw, err := os.ReadFile(seqPath); err == nil { //nolint:gosec // G304: built from the checked session id
		if n, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
			last = n
		}
	}
	if last == 0 {
		if highest := maxSequenceInFile(spoolPath); highest > last {
			last = highest
		}
	}
	return last
}

func maxSequenceInFile(path string) uint64 {
	f, err := os.Open(path) //nolint:gosec // G304: built from the checked session id
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()
	var highest uint64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventBytes+1)
	for sc.Scan() {
		var ev struct {
			Sequence uint64 `json:"sequence"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Sequence > highest {
			highest = ev.Sequence
		}
	}
	return highest
}

// writeSeq replaces the sidecar atomically: a temporary file in the same directory, then a rename.
func writeSeq(path string, seq uint64) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".seq-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.WriteString(strconv.FormatUint(seq, 10))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// Read returns the events of one session in file order. A line that does not parse is skipped, not fatal: the spool is
// appended to by agents' hooks and a torn line must not hide the rest.
func (s *Spool) Read(sessionID string) ([]runtimeevents.Event, error) {
	if !sessionFileName.MatchString(sessionID) {
		return nil, fmt.Errorf("agentevents: session id %q is not a safe file name", sessionID)
	}
	f, err := os.Open(filepath.Join(s.Dir, sessionID+".jsonl")) //nolint:gosec // G304: name checked above
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []runtimeevents.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventBytes+1)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev runtimeevents.Event
		if json.Unmarshal(line, &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

// Prune removes session files not written for maxAge, then the oldest files until the spool is within maxBytes (a zero
// limit is not applied). It returns how many sessions it removed. Run it from the daemon's housekeeping, never from a hook.
func (s *Spool) Prune(now time.Time, maxAge time.Duration, maxBytes int64) (int, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	type file struct {
		session string
		mod     time.Time
		size    int64
	}
	var files []file
	var total int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{strings.TrimSuffix(name, ".jsonl"), info.ModTime(), info.Size()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	removed := 0
	for _, f := range files {
		old := maxAge > 0 && now.Sub(f.mod) > maxAge
		over := maxBytes > 0 && total > maxBytes
		if !old && !over {
			continue
		}
		gone, err := s.removeSession(f.session)
		if err != nil {
			return removed, err
		}
		if !gone {
			continue // a writer holds it; it is not idle, whatever its size
		}
		total -= f.size
		removed++
	}
	return removed, nil
}

// removeSession deletes a session's files unless a writer holds its lock, so a prune can never unlink a file an Append is
// in the middle of. It reports whether the session is gone.
func (s *Spool) removeSession(session string) (bool, error) {
	path := filepath.Join(s.Dir, session+".jsonl")
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // G304: a name read from our own directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	defer func() { _ = f.Close() }()
	unlock, err := tryLockFile(f)
	if err != nil {
		return false, nil //nolint:nilerr // held by a writer: skip, not a failure
	}
	defer unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	_ = os.Remove(filepath.Join(s.Dir, session+".seq"))
	return true, nil
}

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
//
// The spool bounds itself, so a hook that nobody prunes for cannot fill the disk. A session file past MaxSessionBytes loses
// its oldest lines (down to three quarters of the cap) under the same lock as the append that overflowed it, and the number
// of events lost is kept in a ".dropped" sidecar ([Spool.Dropped]). When a session's first event creates its file, whole
// idle sessions older than MaxAge, and the oldest ones beyond MaxTotalBytes, are pruned too.
type Spool struct {
	Dir string
	// MaxSessionBytes caps one session's file (default 4 MiB).
	MaxSessionBytes int64
	// MaxTotalBytes caps the directory (default 256 MiB).
	MaxTotalBytes int64
	// MaxAge removes sessions idle longer than this (default 14 days).
	MaxAge time.Duration
}

const (
	defaultMaxSessionBytes = 4 << 20
	defaultMaxTotalBytes   = 256 << 20
	defaultMaxAge          = 14 * 24 * time.Hour
)

func (s *Spool) limits() (session, total int64, age time.Duration) {
	session, total, age = s.MaxSessionBytes, s.MaxTotalBytes, s.MaxAge
	if session <= 0 {
		session = defaultMaxSessionBytes
	}
	if total <= 0 {
		total = defaultMaxTotalBytes
	}
	if age <= 0 {
		age = defaultMaxAge
	}
	return session, total, age
}

// Append stores ev in its session's file and returns it with Sequence assigned when it was zero.
func (s *Spool) Append(ctx context.Context, ev runtimeevents.Event) (runtimeevents.Event, error) {
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
	_, statErr := os.Stat(path)
	if statErr != nil {
		// A new session file: bound the directory once it exists, after this append has released its lock.
		defer func() { _, _ = s.pruneToLimits(time.Now(), ev.SessionID) }()
	}
	f, unlock, err := openLockedCtx(ctx, path)
	if err != nil {
		return ev, err
	}
	defer func() { _ = f.Close() }()
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
	if err := s.boundSession(f, path); err != nil {
		return ev, err
	}
	return ev, nil
}

// boundSession drops the oldest lines of a session file that grew past its cap. The caller holds the file lock. It rewrites
// in place (truncate, then append the kept tail), so a process killed mid-rewrite can lose spooled events; the spool is a
// best-effort local record, and losing the oldest is the stated policy.
func (s *Spool) boundSession(f *os.File, path string) error {
	maxSession, _, _ := s.limits()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= maxSession {
		return err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: built from the checked session id
	if err != nil {
		return fmt.Errorf("agentevents: bound spool: %w", err)
	}
	lines := bytes.SplitAfter(raw, []byte{'\n'})
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	keepBytes, keepFrom := int64(0), len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if keepBytes+int64(len(lines[i])) > maxSession*3/4 && keepFrom < len(lines) {
			break
		}
		keepBytes += int64(len(lines[i]))
		keepFrom = i
	}
	if keepFrom == 0 {
		return nil
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("agentevents: bound spool: %w", err)
	}
	if _, err := f.Write(bytes.Join(lines[keepFrom:], nil)); err != nil {
		return fmt.Errorf("agentevents: bound spool: %w", err)
	}
	counter := strings.TrimSuffix(path, ".jsonl") + ".dropped"
	return writeCounter(counter, readCounter(counter)+uint64(keepFrom)) //nolint:gosec // G115: a line count is never negative
}

// Dropped is how many events of a session the spool has discarded to stay within its cap.
func (s *Spool) Dropped(sessionID string) uint64 {
	if !sessionFileName.MatchString(sessionID) {
		return 0
	}
	return readCounter(filepath.Join(s.Dir, sessionID+".dropped"))
}

func readCounter(path string) uint64 {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: built from the checked session id
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	return n
}

func writeCounter(path string, n uint64) error { return writeSeq(path, n) }

// pruneToLimits applies the directory limits, never touching the session that caused it.
func (s *Spool) pruneToLimits(now time.Time, keep string) (int, error) {
	_, total, age := s.limits()
	return s.prune(now, age, total, keep)
}

// lastSequence is the highest sequence the session has used. The sidecar is the fast path; when it is missing, unreadable
// or zero, the spool file itself is the truth, so a damaged sidecar can never restart the sequence. (Because the sequence is
// written before the line and replaced by rename, a sidecar that is merely too low can only come from outside damage, which
// is not worth a scan of the file on every append.)
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
// limit is not applied). It returns how many sessions it removed. Append runs it when a session file is first created; a daemon may run it as housekeeping too.
func (s *Spool) Prune(now time.Time, maxAge time.Duration, maxBytes int64) (int, error) {
	return s.prune(now, maxAge, maxBytes, "")
}

func (s *Spool) prune(now time.Time, maxAge time.Duration, maxBytes int64, keep string) (int, error) {
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
		if f.session == keep {
			continue
		}
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
	_ = os.Remove(filepath.Join(s.Dir, session+".dropped"))
	_ = os.Remove(filepath.Join(s.Dir, session+".hookdrops"))
	return true, nil
}

// openLocked opens the session file for appending and takes its lock. A prune can unlink the path between the open and the
// lock (it try-locks, finds the file free, removes it); a writer that then got the lock would be appending to an unlinked
// inode and its event would vanish. So after the lock the open file must still be the one the path names, else it reopens.
func openLocked(path string) (*os.File, func(), error) {
	return openLockedCtx(context.Background(), path)
}

// openLockedCtx is openLocked that stops waiting for the lock when ctx ends.
func openLockedCtx(ctx context.Context, path string) (*os.File, func(), error) {
	for attempt := 0; attempt < 5; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: name checked by the caller, dir is ours
		if err != nil {
			return nil, nil, fmt.Errorf("agentevents: open spool: %w", err)
		}
		unlock, err := lockFileCtx(ctx, f)
		if err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("agentevents: lock spool: %w", err)
		}
		fi, ferr := f.Stat()
		pi, perr := os.Stat(path)
		if ferr == nil && perr == nil && os.SameFile(fi, pi) {
			return f, unlock, nil
		}
		unlock()
		_ = f.Close()
	}
	return nil, nil, errors.New("agentevents: spool file kept changing under the writer")
}

// NoteHookDrop records that a hook gave up on an event (it ran out of its write budget, or the spool refused it): one line
// naming the class is appended to the session's ".hookdrops" file. A single O_APPEND write needs no lock, which matters
// because the usual reason to give up is a lock that is held. It is best effort and returns nothing: the caller is a hook
// that must not be held up by its own bookkeeping.
func (s *Spool) NoteHookDrop(sessionID, class string) {
	if s == nil || s.Dir == "" || !sessionFileName.MatchString(sessionID) || class == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, sessionID+".hookdrops"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: name checked above
	if err != nil {
		return
	}
	_, _ = f.WriteString(class + "\n")
	_ = f.Close()
}

// HookDrops is how many events hooks have given up on for a session, by class.
func (s *Spool) HookDrops(sessionID string) map[string]int {
	out := map[string]int{}
	if !sessionFileName.MatchString(sessionID) {
		return out
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, sessionID+".hookdrops")) //nolint:gosec // G304: name checked above
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out[line]++
		}
	}
	return out
}

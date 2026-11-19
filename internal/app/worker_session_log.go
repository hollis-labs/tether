package app

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Group commit for worker session logs (CW-20261114-0120). Ingest used to
// open, append to and fsync <workspace>/logs/session.log for every output
// frame: about 1 ms of the 1.7 ms a frame cost. A session's log is now kept
// open and fsynced once per sessionLogSyncFrames frames or sessionLogSyncDelay,
// whichever comes first.
//
// Durability contract: SQLite is the record of what was accepted (the cursor
// and the session.output event commit together, as before). session.log is
// the raw byte journal and, on a crash, may lose at most its un-synced tail:
// the last batch, fewer than sessionLogSyncFrames frames written within
// sessionLogSyncDelay. The cursor and events are unaffected. A codex
// permission-source session is the exception: its parser stores byte spans of
// session.log in SQLite and reads them back, so those frames are synced before
// their spans commit, as before.
var (
	sessionLogSyncFrames = 32
	sessionLogSyncDelay  = 100 * time.Millisecond
	// sessionLogOpenLimit bounds the open logs; the least recently written is
	// synced and closed first.
	sessionLogOpenLimit = 256
	// afterSessionLogSync, when set by a test, is told each log's size once it
	// is synced: everything up to it survives a crash.
	afterSessionLogSync func(path string, size int64)
)

type sessionLog struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	end      int64
	dirty    int
	timer    *time.Timer
	lastUsed time.Time
	closed   bool
}

type sessionLogSet struct {
	mu   sync.Mutex
	logs map[string]*sessionLog // by session id
}

// write appends data to the session's log with the log held, then runs then
// (with the open file and the offset data was written at), and syncs at once
// when syncNow, else per the group-commit rule. then may be nil.
func (set *sessionLogSet) write(sessionID, path string, data []byte, syncNow bool, then func(*os.File, int64) error) error {
	var l *sessionLog
	for {
		var err error
		if l, err = set.open(sessionID, path); err != nil {
			return err
		}
		l.mu.Lock()
		if !l.closed {
			break
		}
		l.mu.Unlock() // evicted between open and lock: open it again
	}
	defer l.mu.Unlock()
	offset := l.end
	n, err := l.file.Write(data)
	l.end += int64(n)
	if err != nil {
		return err
	}
	l.lastUsed = time.Now()
	l.dirty++
	if syncNow || l.dirty >= sessionLogSyncFrames {
		if err = l.syncLocked(); err != nil {
			return err
		}
	} else if l.timer == nil {
		l.timer = time.AfterFunc(sessionLogSyncDelay, func() { l.flush() })
	}
	if then != nil {
		return then(l.file, offset)
	}
	return nil
}

// open returns the session's open log, opening it (and evicting the least
// recently used past the limit) when needed.
func (set *sessionLogSet) open(sessionID, path string) (*sessionLog, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if l := set.logs[sessionID]; l != nil && l.path == path {
		return l, nil
	} else if l != nil {
		delete(set.logs, sessionID)
		go l.close()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // G304: path is the hub-owned session log of a session this connection owns
	if err != nil {
		return nil, err
	}
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if set.logs == nil {
		set.logs = map[string]*sessionLog{}
	}
	if len(set.logs) >= sessionLogOpenLimit {
		var oldest string
		var at time.Time
		for id, l := range set.logs {
			l.mu.Lock()
			used := l.lastUsed
			l.mu.Unlock()
			if oldest == "" || used.Before(at) {
				oldest, at = id, used
			}
		}
		victim := set.logs[oldest]
		delete(set.logs, oldest)
		go victim.close()
	}
	l := &sessionLog{path: path, file: f, end: end, lastUsed: time.Now()}
	set.logs[sessionID] = l
	return l, nil
}

func (l *sessionLog) syncLocked() error {
	if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	if l.dirty == 0 || l.closed {
		return nil
	}
	l.dirty = 0
	if err := l.file.Sync(); err != nil {
		return err
	}
	if afterSessionLogSync != nil {
		afterSessionLogSync(l.path, l.end)
	}
	return nil
}

// flush is the delayed sync of the group-commit rule.
func (l *sessionLog) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timer = nil
	if err := l.syncLocked(); err != nil {
		log.Printf("app: sync session log %s: %v", l.path, err)
	}
}

// close syncs and closes the log.
func (l *sessionLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	if err := l.syncLocked(); err != nil {
		log.Printf("app: sync session log %s: %v", l.path, err)
	}
	l.closed = true
	_ = l.file.Close()
}

// closeSession syncs and closes a session's log (its session ended).
func (set *sessionLogSet) closeSession(sessionID string) {
	set.mu.Lock()
	l := set.logs[sessionID]
	delete(set.logs, sessionID)
	set.mu.Unlock()
	if l != nil {
		l.close()
	}
}

// closeAll syncs and closes every open log (shutdown).
func (set *sessionLogSet) closeAll() {
	set.mu.Lock()
	logs := set.logs
	set.logs = nil
	set.mu.Unlock()
	for _, l := range logs {
		l.close()
	}
}

// abandon closes every log without syncing: a test's stand-in for a crash,
// after which only what afterSessionLogSync reported is known to be on disk.
func (set *sessionLogSet) abandon() {
	set.mu.Lock()
	logs := set.logs
	set.logs = nil
	set.mu.Unlock()
	for _, l := range logs {
		l.mu.Lock()
		if l.timer != nil {
			l.timer.Stop()
		}
		l.closed = true
		_ = l.file.Close()
		l.mu.Unlock()
	}
}

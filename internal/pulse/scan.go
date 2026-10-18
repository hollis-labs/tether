// Package pulse reads the local hook snapshot without requiring an agent to
// have been launched by Tether. It never reads transcripts or environments.
package pulse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	re "github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/workerwire"
)

const MaxAgents = 512

type Scanner struct {
	mu         sync.Mutex
	sequence   uint64
	generation uint64
	Directory  string
	Starts     func(context.Context) (map[int]string, error)
}

func New() *Scanner {
	home, _ := os.UserHomeDir()
	root := os.Getenv("TETHER_PULSE_DIR")
	if root == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(home, ".local", "state")
		}
		root = filepath.Join(base, "tether-pulse")
	}
	machine := os.Getenv("TETHER_PULSE_MACHINE")
	if machine == "" {
		machine, _ = os.Hostname()
		machine = strings.SplitN(machine, ".", 2)[0]
	}
	return &Scanner{Directory: filepath.Join(root, machine), Starts: processStarts}
}

type action struct {
	Tool string `json:"tool"`
	At   int64  `json:"at"`
	Done bool   `json:"done"`
}
type document struct {
	Version    int     `json:"v"`
	Tool       string  `json:"tool"`
	SessionID  string  `json:"session_id"`
	PID        int     `json:"pid"`
	ProcStart  string  `json:"proc_start"`
	State      string  `json:"state"`
	UpdatedAt  int64   `json:"updated_at"`
	LastAction *action `json:"last_action"`
}

// Scan validates PID birth times on the source machine. Provider session ID
// remains part of identity: multiple Codex threads may share an app-server PID.
func (s *Scanner) Scan(ctx context.Context, now time.Time) (*workerwire.Activity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation == 0 {
		s.generation = uint64(time.Now().UnixNano())
	}
	entries, err := os.ReadDir(s.Directory)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	batch := &workerwire.Activity{ID: uuid.NewString(), ObservedAt: now.UTC(), Events: []re.Event{}}
	if len(entries) == 0 {
		return batch, nil
	}
	starts, err := s.Starts(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		f, err := os.Open(filepath.Join(s.Directory, entry.Name()))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		var d document
		if len(b) > 64<<10 || json.Unmarshal(b, &d) != nil || d.Version != 1 || d.SessionID == "" || len(d.SessionID) > 200 || d.UpdatedAt <= 0 {
			continue
		}
		if d.Tool != "claude" && d.Tool != "codex" && d.Tool != "agy" {
			continue
		}
		if d.PID <= 0 || d.ProcStart == "" || starts[d.PID] != strings.Join(strings.Fields(d.ProcStart), " ") {
			continue
		}
		id := d.Tool + ":" + d.SessionID
		if seen[id] {
			continue
		}
		seen[id] = true
		state := d.State
		if state == "working" {
			state = "busy"
		}
		if state != "busy" && state != "idle" && state != "waiting" {
			state = "unknown"
		}
		last := time.UnixMilli(d.UpdatedAt).UTC()
		stale := now.Sub(last) > 5*time.Minute || last.After(now.Add(time.Minute))
		confidence := "exact"
		if stale {
			state, confidence = "unknown", "derived"
		}
		// Free-form command targets, prompts, paths and inferred task claims
		// stay local. Transport only the hook's bounded tool name and timing.
		if d.LastAction != nil {
			d.LastAction.Tool = safeTool(d.LastAction.Tool)
		}
		payload, err := json.Marshal(map[string]any{"state": state, "last_activity": last, "last_action": d.LastAction, "stale": stale, "process_verified": true, "confidence": map[string]string{"state": confidence, "session_id": "exact", "alive": "derived"}})
		if err != nil {
			return nil, err
		}
		s.sequence++
		batch.Events = append(batch.Events, re.Event{SchemaVersion: re.SchemaVersion, ID: uuid.NewString(), Kind: re.KindSessionHeartbeat, Time: now.UTC(), App: "tether-pulse", SessionID: id, Sequence: s.sequence, Generation: s.generation, Process: re.Process{PID: d.PID, Provider: d.Tool, ProviderSessionID: d.SessionID}, Source: re.Source{Channel: "plugin-hook", Confidence: re.Confidence(confidence)}, Payload: payload})
		if len(batch.Events) > MaxAgents {
			return nil, fmt.Errorf("pulse scan exceeds %d agents", MaxAgents)
		}
	}
	return batch, nil
}

func safeTool(s string) string {
	if len(s) > 120 {
		return ""
	}
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-/", c) {
			continue
		}
		return ""
	}
	return s
}

// ps lstart is the same portable representation the Python hooks record.
func processStarts(ctx context.Context) (map[int]string, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("pulse process inventory: %w", err)
	}
	starts := map[int]string{}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 6 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err == nil {
			starts[pid] = strings.Join(fields[1:], " ")
		}
	}
	return starts, nil
}

// Package workerwire defines the thin outpost channel. The hub owns sessions;
// worker state describes only local execution custody and replay cursors.
package workerwire

import "encoding/json"

const Protocol = 1
const ConnectPath = "/worker/v1/connect"

type Worker struct {
	Label    string `json:"label"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}
type Session struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	LastSeq  int64  `json:"last_seq"`
	ExitCode *int   `json:"exit_code,omitempty"`
}
type Cursor struct {
	Session string `json:"session"`
	FromSeq int64  `json:"from_seq"`
}
type Frame struct {
	T         string          `json:"t"`
	Proto     int             `json:"proto,omitempty"`
	Worker    *Worker         `json:"worker,omitempty"`
	Caps      json.RawMessage `json:"caps,omitempty"`
	Sessions  []Session       `json:"sessions,omitempty"`
	WorkerID  string          `json:"worker_id,omitempty"`
	Resume    []Cursor        `json:"resume,omitempty"`
	Kill      []string        `json:"kill,omitempty"`
	CmdID     string          `json:"cmd_id,omitempty"`
	Session   string          `json:"session,omitempty"`
	Spec      json.RawMessage `json:"spec,omitempty"`
	PID       int             `json:"pid,omitempty"`
	Workspace string          `json:"workspace,omitempty"`
	Sandboxed bool            `json:"sandboxed"`
	Error     string          `json:"error,omitempty"`
	Data      []byte          `json:"data,omitempty"`
	Stream    string          `json:"stream,omitempty"`
	Seq       int64           `json:"seq,omitempty"`
	FromSeq   int64           `json:"from_seq,omitempty"`
	At        string          `json:"at,omitempty"`
	State     string          `json:"state,omitempty"`
	ExitCode  *int            `json:"exit_code,omitempty"`
	GraceMS   int             `json:"grace_ms,omitempty"`
	Cols      int             `json:"cols,omitempty"`
	Rows      int             `json:"rows,omitempty"`
	TS        int64           `json:"ts,omitempty"`
}

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

// Host is one shim-host directory on the worker. Orphaned means a live host with no live agent child and no
// running session: a leak the worker reaps after a grace period.
type Host struct {
	Dir        string `json:"dir"`
	Session    string `json:"session,omitempty"`
	State      string `json:"state,omitempty"`
	AgeSec     int64  `json:"age_sec"`
	HostPID    int    `json:"host_pid,omitempty"`
	HostAlive  bool   `json:"host_alive"`
	ChildAlive bool   `json:"child_alive"`
	Orphaned   bool   `json:"orphaned"`
}
type Cursor struct {
	Session string `json:"session"`
	FromSeq int64  `json:"from_seq"`
}
type Frame struct {
	Activity  *Activity       `json:"activity,omitempty"`
	Features  []string        `json:"features,omitempty"`
	Bindings  []Binding       `json:"bindings,omitempty"`
	Binding   *Binding        `json:"binding,omitempty"`
	URN       string          `json:"urn,omitempty"`
	Text      string          `json:"text,omitempty"`
	MessageID string          `json:"message_id,omitempty"`
	T         string          `json:"t"`
	Proto     int             `json:"proto,omitempty"`
	Worker    *Worker         `json:"worker,omitempty"`
	Caps      json.RawMessage `json:"caps,omitempty"`
	Sessions  []Session       `json:"sessions,omitempty"`
	Hosts     []Host          `json:"hosts,omitempty"`
	Reaped    []string        `json:"reaped,omitempty"`
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

// Package agentevents turns what an agent CLI reports about itself (Claude hooks, Codex app-server notifications, Agy
// hooks) into harness/core runtimeevents v1.1 events and keeps them in a local per-session JSONL spool.
//
// This is the shared core of the per-agent adapters (CW-20261014-0143). An adapter maps one vendor signal to one catalog
// event with a [Tier] saying how directly the vendor reported it, and hands it to a [Recorder]; the recorder stamps the
// envelope (id, time, subject, process, source) and appends it to a [Spool]. Nothing here reads a vendor, plants a hook or
// talks to the hub: the spool exists so events are captured before any relay does, and the relay is a later change.
//
// The spool is files, not a database table, so it needs no migration. One file per session keeps concurrent writers apart
// (every hook invocation is its own process), and the per-session sequence the envelope requires is assigned under the
// file lock so it stays monotonic across those processes.
package agentevents

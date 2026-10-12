package outpostmail

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Attach runs as the agent's uid: Codex's app-server and tmux are same-uid
// services. No hub credential and no arbitrary command come over this socket.
// Claude's mod uses the same wire and acknowledges $.prompt.submit itself.
func Attach(ctx context.Context, socket string, q Request, out io.Writer) error {
	started, _, err := ProcessIdentity(q.PID)
	if err != nil {
		return err
	}
	q.Started = started
	q.Op = "register"
	q.TTL = 60
	for ctx.Err() == nil {
		actual, _, probe := ProcessIdentity(q.PID)
		if probe != nil || actual != q.Started {
			return nil
		}
		err = attach(ctx, socket, &q, out)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "outpost mail:", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
	return nil
}
func attach(ctx context.Context, socket string, q *Request, out io.Writer) error {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	var mu sync.Mutex
	send := func(v Request) error {
		mu.Lock()
		defer mu.Unlock()
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return json.NewEncoder(c).Encode(v)
	}
	if q.ID != "" {
		q.Op = "heartbeat"
	}
	if err = send(*q); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				// The reader owns q; heartbeat obtains its snapshot with the write lock.
				mu.Lock()
				heartbeat := Request{Op: "heartbeat", ID: q.ID, Capability: q.Capability}
				mu.Unlock()
				if heartbeat.ID != "" {
					_ = send(heartbeat)
				}
			}
		}
	}()
	scan := bufio.NewScanner(c)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		var r Reply
		if err = json.Unmarshal(scan.Bytes(), &r); err != nil {
			return err
		}
		switch r.Op {
		case "error":
			mu.Lock()
			q.ID = ""
			q.Capability = ""
			q.Op = "register"
			mu.Unlock()
			return fmt.Errorf("%s", r.Error)
		case "registered":
			mu.Lock()
			q.ID = r.ID
			q.Capability = r.Capability
			mu.Unlock()
			_ = json.NewEncoder(out).Encode(r)
		case "deliver":
			if q.Adapter == "claude-mod" {
				mu.Lock()
				r.ID = q.ID
				r.Capability = q.Capability
				mu.Unlock()
				if err := json.NewEncoder(out).Encode(r); err != nil {
					return err
				}
				continue
			}
			call, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = ExecuteOptions(call, q.Adapter, q.Handle, r.Text, q.EscapeFirst)
			cancel()
			ack := Request{Op: "ack", CmdID: r.CmdID}
			if err != nil {
				ack.Error = err.Error()
			}
			if err = send(ack); err != nil {
				return err
			}
		}
	}
	return scan.Err()
}

func Execute(ctx context.Context, adapter, handle, text string) error {
	return ExecuteOptions(ctx, adapter, handle, text, false)
}

// Native mods cannot import socket libraries; this one-shot path uses the
// per-registration capability and sends no hub credential.
func Acknowledge(ctx context.Context, socket string, q Request) error {
	c, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	q.Op = "ack"
	if err = json.NewEncoder(c).Encode(q); err != nil {
		return err
	}
	var r Reply
	if err = json.NewDecoder(c).Decode(&r); err != nil {
		return err
	}
	if r.Error != "" {
		return fmt.Errorf("%s", r.Error)
	}
	return nil
}
func ExecuteOptions(ctx context.Context, adapter, handle, text string, escapeFirst bool) error {
	if handle == "" || strings.HasPrefix(handle, "-") {
		return fmt.Errorf("invalid adapter handle")
	}
	run := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, b)
		}
		return nil
	}
	switch adapter {
	case "codex-queue":
		return run("codex", "queue", "--thread", handle, "--message", text)
	case "tmux-paste":
		if !strings.HasPrefix(handle, "%") {
			return fmt.Errorf("tmux handle must be a pane id")
		}
		_ = run("tmux", "send-keys", "-t", handle, "-X", "cancel")
		if escapeFirst {
			if err := run("tmux", "send-keys", "-t", handle, "Escape"); err != nil {
				return err
			}
		}
		buffer := "tether-mail-" + fmt.Sprint(os.Getpid())
		cmd := exec.CommandContext(ctx, "tmux", "load-buffer", "-b", buffer, "-")
		cmd.Stdin = strings.NewReader(text)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux load: %w: %s", err, b)
		}
		defer func() { _ = run("tmux", "delete-buffer", "-b", buffer) }()
		if err := run("tmux", "paste-buffer", "-b", buffer, "-p", "-t", handle); err != nil {
			return err
		}
		return run("tmux", "send-keys", "-t", handle, "C-m")
	default:
		return fmt.Errorf("adapter %q requires its native socket client", adapter)
	}
}

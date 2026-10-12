package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
)

var (
	mailboxJSONFlag     bool
	mailboxAfterSeq     int64
	mailboxKind         string
	mailboxThread       string
	mailboxUnreadOnly   bool
	mailboxWatchBackoff = time.Second
	mailboxWatchMaxWait = 30 * time.Second
)

var mailboxCmd = &cobra.Command{
	Use:   "mailbox",
	Short: "Mailbox streaming commands",
}

var mailboxWatchCmd = &cobra.Command{
	Use:   "watch <urn>",
	Short: "Stream new mailbox messages without marking them delivered or read",
	Long: `Stream a recipient's mailbox over SSE until interrupted.

Reads are side-effect free, like "messages list": nothing is marked delivered or
read. Without --after-seq only messages that arrive after the watch starts are
shown. The command reconnects with backoff, resumes from the last seen seq and
skips anything it already printed. With --json each delivery is one line with
the "messages list --json" fields plus "seq"; a dropped-history notice is a line
{"event":"gap","reason":...,"high_water_seq":...}.

A hub without the route fails fast, so callers can fall back to polling.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		var after *int64
		if cmd.Flags().Changed("after-seq") {
			after = &mailboxAfterSeq
		}
		return classifyErr(watchMailbox(cmdCtx(cmd), c, client.MailboxWatchQuery{
			To: args[0], Kind: mailboxKind, ThreadID: mailboxThread, UnreadOnly: mailboxUnreadOnly,
		}, after))
	},
}

// watchMailbox is the resume loop around Client.MailboxWatch. cursor is the
// last seq seen (or the "synchronized" position when started from now); it
// only moves forward, which is what dedupes a replayed window.
func watchMailbox(ctx context.Context, c *client.Client, q client.MailboxWatchQuery, cursor *int64) error {
	wait := mailboxWatchBackoff
	for ctx.Err() == nil {
		q.AfterSeq = cursor
		events, errCh, err := c.MailboxWatch(ctx, q)
		if err != nil {
			if !errors.Is(err, client.ErrDaemonUnreachable) && ctx.Err() == nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "mailbox watch: %v; retrying in %s\n", err, wait)
		} else {
			for ev := range events {
				wait = mailboxWatchBackoff
				if err := handleMailboxEvent(ev, &cursor); err != nil {
					return err
				}
			}
			if err := <-errCh; err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "mailbox watch: %v; reconnecting in %s\n", err, wait)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(wait*2, mailboxWatchMaxWait)
	}
	return nil
}

func handleMailboxEvent(ev client.MailboxWatchEvent, cursor **int64) error {
	advance := func(seq int64) {
		if *cursor == nil || seq > **cursor {
			s := seq
			*cursor = &s
		}
	}
	switch ev.Type {
	case "message":
		if *cursor != nil && ev.Seq <= **cursor {
			return nil
		}
		advance(ev.Seq)
		if mailboxJSONFlag {
			b, err := json.Marshal(ev.Message)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(os.Stdout, "%s\n", b)
			return err
		}
		m := ev.Message
		subject := m.Subject
		if subject == "" {
			subject = payloadSubject(m.Payload)
		}
		_, err := fmt.Fprintf(os.Stdout, "%d\t%s\t%s\t%s\t%s\n", m.Seq, m.ID, m.Kind, m.From, subject)
		return err
	case "synchronized":
		advance(ev.Seq)
	case "gap":
		// "ahead" means our cursor is past the hub's newest row; continue from
		// the hub's head. History between is unknowable, so say so.
		if mailboxJSONFlag {
			b, _ := json.Marshal(map[string]any{"event": "gap", "reason": ev.Reason, "high_water_seq": ev.Seq})
			fmt.Fprintf(os.Stdout, "%s\n", b)
		} else {
			fmt.Fprintf(os.Stderr, "gap: %s (high_water_seq=%d)\n", ev.Reason, ev.Seq)
		}
		if ev.Reason == "ahead" {
			s := ev.Seq
			*cursor = &s
		}
	}
	return nil
}

func init() {
	mailboxWatchCmd.Flags().BoolVar(&mailboxJSONFlag, "json", false, "print one JSON object per line")
	mailboxWatchCmd.Flags().Int64Var(&mailboxAfterSeq, "after-seq", 0, "resume strictly after this seq (default: only new messages)")
	mailboxWatchCmd.Flags().StringVar(&mailboxKind, "kind", "", "comma-separated kind filter")
	mailboxWatchCmd.Flags().StringVar(&mailboxThread, "thread", "", "thread ID filter")
	mailboxWatchCmd.Flags().BoolVar(&mailboxUnreadOnly, "unread-only", false, "only messages not yet marked read")
	mailboxCmd.AddCommand(mailboxWatchCmd)
}

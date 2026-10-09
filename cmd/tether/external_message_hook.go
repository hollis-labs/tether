package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/spf13/cobra"
)

const externalHookInputLimit = 1 << 20

type externalMessageHookOptions struct {
	recipient string
	timeout   time.Duration
	wait      time.Duration
	interval  time.Duration
}

// Only lifecycle fields are used. Prompts, transcripts, identity claims and
// credential paths in hook input never select authority or become output.
type externalMessageHookInput struct {
	Event      string `json:"hook_event_name"`
	StopActive *bool  `json:"stop_hook_active"`
}

type externalMessageHookOutput struct {
	Decision   string `json:"decision,omitempty"`
	Reason     string `json:"reason,omitempty"`
	SystemNote string `json:"systemMessage,omitempty"`
	Context    *struct {
		Event string `json:"hookEventName"`
		Text  string `json:"additionalContext"`
	} `json:"hookSpecificOutput,omitempty"`
}

type externalMessageLister interface {
	MessageList(context.Context, string, string, string, bool, bool, int, int) (client.MessageListResult, error)
}

// Exit 2 has a special meaning for Claude asyncRewake hooks. Configuration,
// input, authorization and transport errors must never use this exit code.
type externalMessageRewake struct{ reminder string }

func (e externalMessageRewake) Error() string { return e.reminder }
func (externalMessageRewake) ExitCode() int   { return 2 }

func newExternalMessageHookCommand() *cobra.Command {
	opts := externalMessageHookOptions{}
	cmd := &cobra.Command{
		Use:           "claude-hook",
		Short:         "Poll unread messages without consuming them and emit Claude hook feedback",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			input, err := readExternalMessageHookInput(cmd.InOrStdin())
			if err != nil {
				return err
			}
			if err := validateExternalMessageHookOptions(opts, input); err != nil {
				return err
			}
			if input.Event == "Stop" && *input.StopActive {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(externalMessageHookOutput{})
			}
			c, err := newDaemonClient(catalogPath)
			if err != nil {
				return errors.New("tether message polling unavailable: check the configured catalog and credentials")
			}
			return runExternalMessageHook(cmd.Context(), c, input, opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&opts.recipient, "as", "", "explicit mailbox recipient URN; daemon authorization still applies")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 2*time.Second, "per-request timeout (maximum 10s)")
	cmd.Flags().DurationVar(&opts.wait, "wait", 0, "bounded one-shot SessionStart wait for unread mail; exits 2 for asyncRewake (maximum 10m)")
	cmd.Flags().DurationVar(&opts.interval, "interval", 5*time.Second, "poll interval with --wait (1s to 1m)")
	return cmd
}

func readExternalMessageHookInput(r io.Reader) (externalMessageHookInput, error) {
	var input externalMessageHookInput
	data, err := io.ReadAll(io.LimitReader(r, externalHookInputLimit+1))
	if err != nil || len(data) > externalHookInputLimit || json.Unmarshal(data, &input) != nil {
		return input, errors.New("tether message polling requires one hook JSON object of at most 1 MiB")
	}
	switch input.Event {
	case "SessionStart", "UserPromptSubmit":
	case "Stop":
		if input.StopActive == nil {
			return input, errors.New("tether Stop polling requires stop_hook_active")
		}
	default:
		return input, errors.New("tether message polling supports SessionStart, UserPromptSubmit and Stop only")
	}
	return input, nil
}

func validateExternalMessageHookOptions(opts externalMessageHookOptions, input externalMessageHookInput) error {
	addr, err := messaging.ParseURN(opts.recipient)
	if err != nil || addr.Kind == messaging.KindGroup {
		return errors.New("tether message polling requires --as with a mailbox recipient URN")
	}
	if opts.timeout <= 0 || opts.timeout > 10*time.Second || opts.wait < 0 || opts.wait > 10*time.Minute || opts.interval < time.Second || opts.interval > time.Minute {
		return errors.New("tether message polling requires timeout in (0,10s], wait in [0,10m], and interval in [1s,1m]")
	}
	if opts.wait > 0 && input.Event != "SessionStart" {
		return errors.New("tether --wait is only supported for a SessionStart asyncRewake hook")
	}
	return nil
}

func runExternalMessageHook(ctx context.Context, c externalMessageLister, input externalMessageHookInput, opts externalMessageHookOptions, out io.Writer) error {
	if err := validateExternalMessageHookOptions(opts, input); err != nil {
		return err
	}
	if input.Event == "Stop" && (input.StopActive == nil || *input.StopActive) {
		return json.NewEncoder(out).Encode(externalMessageHookOutput{})
	}
	if opts.wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.wait)
		defer cancel()
	}
	for {
		requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		// List is read-only. Inbox would stamp delivered_at and could lose
		// messages before the model actually handles them.
		page, err := c.MessageList(requestCtx, opts.recipient, "", "", false, true, 1, 0)
		cancel()
		if err != nil {
			if opts.wait > 0 && ctx.Err() == context.DeadlineExceeded {
				return nil
			}
			// API errors can contain reflected request/body details. Keep hook
			// diagnostics fixed, and never retry a denial using another identity.
			return errors.New("tether message polling unavailable: check daemon connectivity and mailbox read authorization")
		}
		if page.Total < 0 {
			return errors.New("tether message polling received an invalid unread count")
		}
		count := max(page.Total, len(page.Messages))
		if count > 0 {
			reminder := fmt.Sprintf("Tether has %d unread message(s) in your configured mailbox. Check them with the existing authorized Tether message interface and handle them within current permissions. Mark read only after handling; this reminder does not acknowledge delivery.", count)
			if opts.wait > 0 {
				return externalMessageRewake{reminder: reminder}
			}
			result := externalMessageHookOutput{SystemNote: fmt.Sprintf("Tether: %d unread message(s)", count)}
			if input.Event == "Stop" {
				result.Decision, result.Reason = "block", reminder
			} else {
				result.Context = &struct {
					Event string `json:"hookEventName"`
					Text  string `json:"additionalContext"`
				}{input.Event, reminder}
			}
			return json.NewEncoder(out).Encode(result)
		}
		if opts.wait == 0 {
			return json.NewEncoder(out).Encode(externalMessageHookOutput{})
		}
		timer := time.NewTimer(opts.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if ctx.Err() == context.DeadlineExceeded {
				return nil
			}
			return errors.New("tether message polling canceled")
		case <-timer.C:
		}
	}
}

func init() {
	messagesCmd.AddCommand(newExternalMessageHookCommand())
}

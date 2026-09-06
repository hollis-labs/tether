package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
)

var (
	messageJSONFlag bool

	messageSendFrom        string
	messageSendTo          string
	messageSendKind        string
	messageSendChannel     string
	messageSendThread      string
	messageSendInReplyTo   string
	messageSendPayloadJSON string
	messageSendSubject     string
	messageSendMetadata    []string
	messageNotifyUrgency   string
	messageNotifySession   string
	messageNotifyNoWake    bool
	messageNotifyWakeText  string

	messageFilterKind     string
	messageFilterThread   string
	messageIncludeArchive bool
	messageUnreadOnly     bool
	messageLimit          int
	messageOffset         int

	messageAs string
)

var messagesCmd = &cobra.Command{
	Use:     "messages",
	Aliases: []string{"message"},
	Short:   "Mailbox messaging commands",
	Long: `Send and inspect Tether mailbox messages.

The destructive agent-pull path is "messages inbox": returned envelopes are
marked delivered and will not appear in a later inbox pull. Use "messages list"
for repeatable operator/UI reads.`,
}

var messageSendCmd = &cobra.Command{
	Use:   "send [body|-]",
	Short: "Send a message envelope",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(messageSendFrom) == "" {
			return validationErr("messages send: --from is required")
		}
		if strings.TrimSpace(messageSendTo) == "" {
			return validationErr("messages send: --to is required")
		}
		payload, err := buildMessagePayload(args)
		if err != nil {
			return validationErr("messages send: %v", err)
		}
		metadata, err := parseMetadata(messageSendMetadata)
		if err != nil {
			return validationErr("messages send: %v", err)
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		sent, err := c.MessageSend(cmdCtx(cmd), client.MessageSendRequest{
			Kind:        messageSendKind,
			Channel:     messageSendChannel,
			From:        messageSendFrom,
			To:          messageSendTo,
			ThreadID:    messageSendThread,
			InReplyTo:   messageSendInReplyTo,
			Payload:     payload,
			ContentType: "application/json",
			Metadata:    metadata,
		})
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(sent)
		}
		fmt.Printf("sent: %s\n", sent.ID)
		return nil
	},
}

var messageNotifyCmd = &cobra.Command{
	Use:   "notify [body|-]",
	Short: "Send a message and wake-inject a live recipient session when possible",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(messageSendFrom) == "" {
			return validationErr("messages notify: --from is required")
		}
		if strings.TrimSpace(messageSendTo) == "" {
			return validationErr("messages notify: --to is required")
		}
		payload, err := buildMessagePayload(args)
		if err != nil {
			return validationErr("messages notify: %v", err)
		}
		metadata, err := parseMetadata(messageSendMetadata)
		if err != nil {
			return validationErr("messages notify: %v", err)
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		wake := !messageNotifyNoWake
		out, err := c.MessageNotify(cmdCtx(cmd), client.MessageNotifyRequest{
			MessageSendRequest: client.MessageSendRequest{
				Kind:        messageSendKind,
				Channel:     messageSendChannel,
				From:        messageSendFrom,
				To:          messageSendTo,
				ThreadID:    messageSendThread,
				InReplyTo:   messageSendInReplyTo,
				Payload:     payload,
				ContentType: "application/json",
				Metadata:    metadata,
			},
			Urgency:   messageNotifyUrgency,
			SessionID: messageNotifySession,
			Wake:      &wake,
			WakeText:  messageNotifyWakeText,
		})
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(out)
		}
		status := "stored"
		if out.WakeDelivered {
			status = "wake-delivered"
		} else if out.WakeAttempted {
			status = "wake-failed"
		}
		fmt.Printf("notified: %s %s unread=%d", out.Message.ID, status, out.UnreadCount)
		if out.SessionID != "" {
			fmt.Printf(" session=%s", out.SessionID)
		}
		if out.WakeError != "" {
			fmt.Printf(" wake_error=%q", out.WakeError)
		}
		fmt.Println()
		return nil
	},
}

var messageGetCmd = &cobra.Command{
	Use:   "get <message-id>",
	Short: "Get one message by ID",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if messageAs == "" {
			return validationErr("messages get: --as <sender-or-recipient-urn> is required")
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		msg, err := c.MessageGet(cmdCtx(cmd), args[0], messageAs)
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(msg)
		}
		printMessages([]client.MessageEnvelopeDTO{msg})
		return nil
	},
}

var messageInboxCmd = &cobra.Command{
	Use:   "inbox <to-urn>",
	Short: "Pull undelivered messages for a recipient (marks delivered)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		msgs, err := c.MessageInbox(cmdCtx(cmd), args[0], messageFilterKind, messageFilterThread)
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(map[string]any{"messages": msgs, "count": len(msgs)})
		}
		printMessages(msgs)
		return nil
	},
}

var messageListCmd = &cobra.Command{
	Use:   "list <to-urn>",
	Short: "List recipient messages non-destructively",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		page, err := c.MessageList(cmdCtx(cmd), args[0], messageFilterKind, messageFilterThread, messageIncludeArchive, messageUnreadOnly, messageLimit, messageOffset)
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(page)
		}
		printMessages(page.Messages)
		if page.Total > len(page.Messages) {
			fmt.Fprintf(os.Stderr, "showing %d of %d (limit=%d offset=%d)\n", len(page.Messages), page.Total, page.Limit, page.Offset)
		}
		return nil
	},
}

var messageThreadCmd = &cobra.Command{
	Use:   "thread <thread-id>",
	Short: "List messages in a thread",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if messageAs == "" {
			return validationErr("messages thread: --as <sender-or-recipient-urn> is required")
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		msgs, err := c.MessageThread(cmdCtx(cmd), args[0], messageAs, "")
		if err != nil {
			return classifyErr(err)
		}
		if messageJSONFlag {
			return printJSON(map[string]any{"messages": msgs, "count": len(msgs)})
		}
		printMessages(msgs)
		return nil
	},
}

var messageConsumeCmd = &cobra.Command{
	Use:   "consume <message-id>",
	Short: "Mark a message consumed by its recipient",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if messageAs == "" {
			return validationErr("messages consume: --as <recipient-urn> is required")
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		if err := c.MessageConsume(cmdCtx(cmd), args[0], messageAs); err != nil {
			return classifyErr(err)
		}
		fmt.Printf("consumed: %s\n", args[0])
		return nil
	},
}

var messageCancelCmd = &cobra.Command{
	Use:   "cancel <message-id>",
	Short: "Cancel a pending message",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		if err := c.MessageCancel(cmdCtx(cmd), args[0]); err != nil {
			return classifyErr(err)
		}
		fmt.Printf("canceled: %s\n", args[0])
		return nil
	},
}

var messageReadCmd = &cobra.Command{
	Use:   "read <message-id>",
	Short: "Mark a message read by its recipient",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMessageRecipientAction(cmd, args[0], "read")
	},
}

var messageArchiveCmd = &cobra.Command{
	Use:   "archive <message-id>",
	Short: "Archive a message for its recipient",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMessageRecipientAction(cmd, args[0], "archive")
	},
}

var messageUnarchiveCmd = &cobra.Command{
	Use:   "unarchive <message-id>",
	Short: "Restore an archived message",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runMessageRecipientAction(cmd, args[0], "unarchive")
	},
}

func runMessageRecipientAction(cmd *cobra.Command, id, action string) error {
	if messageAs == "" {
		return validationErr("messages %s: --as <recipient-urn> is required", action)
	}
	c, err := newDaemonClient(catalogPath)
	if err != nil {
		return classifyErr(err)
	}
	switch action {
	case "read":
		err = c.MessageMarkRead(cmdCtx(cmd), id, messageAs)
	case "archive":
		err = c.MessageArchive(cmdCtx(cmd), id, messageAs)
	case "unarchive":
		err = c.MessageUnarchive(cmdCtx(cmd), id, messageAs)
	default:
		err = fmt.Errorf("unknown message action %q", action)
	}
	if err != nil {
		return classifyErr(err)
	}
	fmt.Printf("%s: %s\n", action, id)
	return nil
}

func buildMessagePayload(args []string) (json.RawMessage, error) {
	if messageSendPayloadJSON != "" {
		if !json.Valid([]byte(messageSendPayloadJSON)) {
			return nil, fmt.Errorf("--payload-json is not valid JSON")
		}
		return json.RawMessage(messageSendPayloadJSON), nil
	}
	body := ""
	if len(args) == 1 {
		if args[0] == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("read stdin: %w", err)
			}
			body = string(b)
		} else {
			body = args[0]
		}
	}
	payload := map[string]string{}
	if messageSendSubject != "" {
		payload["subject"] = messageSendSubject
	}
	if body != "" {
		payload["body"] = body
	}
	if len(payload) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func parseMetadata(items []string) (map[string]string, error) {
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		k, v, ok := strings.Cut(item, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("--metadata values must be key=value")
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

func printMessages(msgs []client.MessageEnvelopeDTO) {
	if len(msgs) == 0 {
		fmt.Println("(no messages)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tFROM\tTO\tCREATED\tSUBJECT")
	for _, m := range msgs {
		subject := m.Subject
		if subject == "" {
			subject = payloadSubject(m.Payload)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.ID, m.Kind, m.From, m.To, m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), subject)
	}
	_ = tw.Flush()
}

func payloadSubject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"subject", "title", "summary", "body", "text", "message"} {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func init() {
	messagesCmd.PersistentFlags().BoolVar(&messageJSONFlag, "json", false, "print JSON")

	messageSendCmd.Flags().StringVar(&messageSendFrom, "from", "", "sender URN")
	messageSendCmd.Flags().StringVar(&messageSendTo, "to", "", "recipient URN")
	messageSendCmd.Flags().StringVar(&messageSendKind, "kind", "notice", "message kind: request, response, notice, status_update, handoff, escalation")
	messageSendCmd.Flags().StringVar(&messageSendChannel, "channel", "", "optional UX channel")
	messageSendCmd.Flags().StringVar(&messageSendThread, "thread", "", "thread ID")
	messageSendCmd.Flags().StringVar(&messageSendInReplyTo, "in-reply-to", "", "parent message ID")
	messageSendCmd.Flags().StringVar(&messageSendPayloadJSON, "payload-json", "", "raw JSON payload body")
	messageSendCmd.Flags().StringVar(&messageSendSubject, "subject", "", "subject for generated JSON payload")
	messageSendCmd.Flags().StringArrayVar(&messageSendMetadata, "metadata", nil, "metadata key=value; repeatable")
	for _, c := range []*cobra.Command{messageNotifyCmd} {
		c.Flags().StringVar(&messageSendFrom, "from", "", "sender URN")
		c.Flags().StringVar(&messageSendTo, "to", "", "recipient URN")
		c.Flags().StringVar(&messageSendKind, "kind", "notice", "message kind: request, response, notice, status_update, handoff, escalation")
		c.Flags().StringVar(&messageSendChannel, "channel", "", "optional UX channel")
		c.Flags().StringVar(&messageSendThread, "thread", "", "thread ID")
		c.Flags().StringVar(&messageSendInReplyTo, "in-reply-to", "", "parent message ID")
		c.Flags().StringVar(&messageSendPayloadJSON, "payload-json", "", "raw JSON payload body")
		c.Flags().StringVar(&messageSendSubject, "subject", "", "subject for generated JSON payload")
		c.Flags().StringArrayVar(&messageSendMetadata, "metadata", nil, "metadata key=value; repeatable")
	}
	messageNotifyCmd.Flags().StringVar(&messageNotifyUrgency, "urgency", "normal", "urgency: very-low, low, normal, high")
	messageNotifyCmd.Flags().StringVar(&messageNotifySession, "session", "", "explicit live session ID to wake")
	messageNotifyCmd.Flags().BoolVar(&messageNotifyNoWake, "no-wake", false, "store message but skip session wake injection")
	messageNotifyCmd.Flags().StringVar(&messageNotifyWakeText, "wake-text", "", "override daemon-generated wake text")

	for _, c := range []*cobra.Command{messageInboxCmd, messageListCmd} {
		c.Flags().StringVar(&messageFilterKind, "kind", "", "comma-separated kind filter")
		c.Flags().StringVar(&messageFilterThread, "thread", "", "thread ID filter")
	}
	messageListCmd.Flags().BoolVar(&messageIncludeArchive, "include-archived", false, "include archived messages")
	messageListCmd.Flags().BoolVar(&messageUnreadOnly, "unread-only", false, "only unread messages")
	messageListCmd.Flags().IntVar(&messageLimit, "limit", 0, "max results, clamped by daemon")
	messageListCmd.Flags().IntVar(&messageOffset, "offset", 0, "pagination offset")

	for _, c := range []*cobra.Command{messageConsumeCmd, messageReadCmd, messageArchiveCmd, messageUnarchiveCmd} {
		c.Flags().StringVar(&messageAs, "as", "", "recipient URN performing the action")
	}
	for _, c := range []*cobra.Command{messageGetCmd, messageThreadCmd} {
		c.Flags().StringVar(&messageAs, "as", "", "sender or recipient URN claiming this read (T05/ADR 0045)")
	}

	messagesCmd.AddCommand(
		messageSendCmd,
		messageNotifyCmd,
		messageGetCmd,
		messageInboxCmd,
		messageListCmd,
		messageThreadCmd,
		messageConsumeCmd,
		messageCancelCmd,
		messageReadCmd,
		messageArchiveCmd,
		messageUnarchiveCmd,
	)
}

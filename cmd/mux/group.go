package main

// group.go — CLI surface for v060-05 group messaging (T-v060-05-06).
// Mirrors the HTTP routes via the typed *client.GroupsClient; no
// in-process daemon dependency.
//
// Subcommands implemented here:
//
//	mux group create   --name <n> [--description <d>] [--category <c>] [--capability <cap>...]
//	mux group list     [--mine]            (--mine needs --as on v1)
//	mux group show     <urn>               [--json]
//	mux group invite   <urn> <member_urn>  [--role member|moderator] [--as <urn>]
//	mux group kick     <urn> <member_urn>  [--as <urn>]
//	mux group leave    <urn>               [--as <urn>]
//	mux group post     <urn> <body>        [--thread <id>] [--as <urn>]
//	mux group read     <urn>               [--since-seq N] [--thread <id>] [--mark-read] [--as <urn>] [--json]
//	mux group mentions                     [--since <RFC3339>] [--limit N] [--as <urn>] [--json]
//	mux group archive  <urn>               [--as <urn>]
//
// Caller-identity surrogate. v060-05 has no token auth (lands in
// v060-03). Every subcommand that needs an "as" identity reads the
// --as flag, falling back to the MUX_CALLER_URN env var so shells can
// avoid repeating it. If both are unset on a subcommand that needs an
// identity, the command errors with validation exit code 2.
//
// Exit-code wiring reuses exitErr / classifyErr from registry.go: the
// same {0=ok, 1=not_found, 2=invalid_request, 3=unreachable,
// 4=other} mapping applies to group operations too.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
)

// callerEnvKey is the env var read as the fallback for --as. Mirrors
// the lighter-weight env convention used elsewhere in the CLI (no
// keychain or token store yet — that's v060-03's beat).
const callerEnvKey = "MUX_CALLER_URN"

// groupsClient returns a *client.GroupsClient via the existing
// registryClientFactory (test seam). Sharing the factory means group
// tests can inject the same httptest.Server-backed client without
// duplicating wiring.
func groupsClient() (*client.GroupsClient, error) {
	c, err := registryClientFactory()
	if err != nil {
		return nil, err
	}
	return c.Groups(), nil
}

// callerURN resolves the caller-identity URN. Priority:
//  1. --as flag (group-cmd-scoped global var)
//  2. MUX_CALLER_URN env var
//
// Returns ("", nil) when both are unset; subcommand decides whether
// the missing value is a validation error.
func callerURN() string {
	if groupAsFlag != "" {
		return strings.TrimSpace(groupAsFlag)
	}
	return strings.TrimSpace(os.Getenv(callerEnvKey))
}

// ─── flags ───────────────────────────────────────────────────────────────

var (
	// Shared across subcommands.
	groupAsFlag   string
	groupJSONFlag bool

	// create flags.
	groupCreateName        string
	groupCreateDescription string
	groupCreateCategory    string
	groupCreateCapability  []string

	// list flag.
	groupListMine bool

	// invite flag.
	groupInviteRole string

	// post flag.
	groupPostThread string

	// read flags.
	groupReadSinceSeq int64
	groupReadThread   string
	groupReadMarkRead bool

	// mentions flags.
	groupMentionsSince string
	groupMentionsLimit int
)

// ─── parent ──────────────────────────────────────────────────────────────

var groupCmd = &cobra.Command{
	Use:   "group",
	Short: "Group messaging (create / invite / post / read / mentions / archive)",
	Long: `Operate against the Mux federation directory's group-mailbox surface.

Groups are a registry kind (msg://group/<authority>/grp_<10alnum>); membership
is a sibling table; messages are stored mailbox-pull (one row per send, not
N rows per member).

The daemon parses '@' mentions on send and emits notices to the mentioned
URN's personal inbox. '!' and ':' are reserved-namespace, agent-side — the
daemon transports them verbatim. See the symbol-vocabulary doc.

Caller identity (--as / MUX_CALLER_URN) is the v060-05 auth surrogate;
v060-03 token auth will replace it.`,
}

// ─── create ──────────────────────────────────────────────────────────────

var groupCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new group (you become the owner)",
	RunE: func(cmd *cobra.Command, args []string) error {
		if groupCreateName == "" {
			return validationErr("group create: --name is required")
		}
		creator := callerURN()
		if creator == "" {
			return validationErr("group create: --as <urn> or $%s is required (v060-05 auth surrogate)", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.Create(cmdCtx(cmd), client.CreateGroupRequest{
			DisplayName:  groupCreateName,
			Description:  groupCreateDescription,
			Role:         groupCreateCategory,
			Capabilities: groupCreateCapability,
			CreatorURN:   creator,
		})
		if err != nil {
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		printGroupProfile(out)
		return nil
	},
}

// ─── list ────────────────────────────────────────────────────────────────

var groupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List groups you belong to (--mine)",
	Long: `List groups for the caller URN.

v1 surface: --mine (combined with --as / MUX_CALLER_URN) is the only
supported mode. Listing groups for an arbitrary URN is not implemented
v1; it lands when token auth + access-control models are in place.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if !groupListMine {
			return validationErr("group list: --mine is required v1 (other modes not implemented)")
		}
		caller := callerURN()
		if caller == "" {
			return validationErr("group list: --as <urn> or $%s is required for --mine", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.ListForMember(cmdCtx(cmd), caller)
		if err != nil {
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		printGroupTable(out)
		return nil
	},
}

// ─── show ────────────────────────────────────────────────────────────────

var groupShowCmd = &cobra.Command{
	Use:   "show <urn>",
	Short: "Show a group's Profile by URN",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.Lookup(cmdCtx(cmd), args[0])
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				fmt.Fprintf(os.Stderr, "group: not found: %s\n", args[0])
				return &exitErr{code: 1, err: err}
			}
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		printGroupProfile(out)
		return nil
	},
}

// ─── invite ──────────────────────────────────────────────────────────────

var groupInviteCmd = &cobra.Command{
	Use:   "invite <group_urn> <member_urn>",
	Short: "Add a member to a group (--role member|moderator)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group invite: --as <urn> or $%s is required", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.AddMember(cmdCtx(cmd), args[0], args[1], caller, registry.MemberRole(groupInviteRole))
		if err != nil {
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		fmt.Printf("invited %s into %s (role=%s)\n", out.MemberURN, out.GroupURN, out.Role)
		return nil
	},
}

// ─── kick ────────────────────────────────────────────────────────────────

var groupKickCmd = &cobra.Command{
	Use:   "kick <group_urn> <member_urn>",
	Short: "Remove a member from a group",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group kick: --as <urn> or $%s is required", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		if err := gc.RemoveMember(cmdCtx(cmd), args[0], args[1], caller); err != nil {
			return classifyErr(err)
		}
		fmt.Printf("kicked %s from %s\n", args[1], args[0])
		return nil
	},
}

// ─── leave ───────────────────────────────────────────────────────────────

var groupLeaveCmd = &cobra.Command{
	Use:   "leave <group_urn>",
	Short: "Leave a group (self-remove)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group leave: --as <urn> or $%s is required", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		if err := gc.Leave(cmdCtx(cmd), args[0], caller); err != nil {
			return classifyErr(err)
		}
		fmt.Printf("left %s\n", args[0])
		return nil
	},
}

// ─── post ────────────────────────────────────────────────────────────────

var groupPostCmd = &cobra.Command{
	Use:   "post <group_urn> <body | ->",
	Short: "Post a message to a group (body via stdin if '-')",
	Long: `Post a message body to a group. Pass '-' to read the body from stdin.

The body is wrapped as a JSON payload ` + "`{\"text\": <body>}`" + `. For
structured payloads or non-text content, use the MCP tool or the typed
client directly.

Symbol vocabulary (v060-05 D6):
  '@<urn>' or '@<display_name>' = mention (daemon-parsed, emits notices)
  '!<command> ...'              = reserved namespace, agent-side
  ':<directive> ...'            = reserved namespace, directives-package
  Escape with backslash: '\@', '\!', '\:' send the symbol literally.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group post: --as <urn> or $%s is required", callerEnvKey)
		}
		body := args[1]
		if body == "-" {
			b, err := io.ReadAll(os.Stdin)
			if err != nil {
				return validationErr("group post: read stdin: %v", err)
			}
			body = string(b)
		}
		payload, err := json.Marshal(map[string]string{"text": body})
		if err != nil {
			return validationErr("group post: marshal body: %v", err)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.Send(cmdCtx(cmd), args[0], client.SendGroupRequest{
			From:     caller,
			Kind:     "message",
			ThreadID: groupPostThread,
			Payload:  payload,
		})
		if err != nil {
			// Special-case the ambiguous-mention error so users see the
			// candidates list inline rather than digging through a wrapped
			// error string.
			var amb *registry.ErrAmbiguousMention
			if errors.As(err, &amb) {
				fmt.Fprintf(os.Stderr, "ambiguous mention %q — re-issue using one of:\n", amb.Token)
				for _, c := range amb.Candidates {
					fmt.Fprintf(os.Stderr, "  - %s\n", c)
				}
				return &exitErr{code: 2, err: err}
			}
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		fmt.Printf("posted: message_id=%s group_seq=%d\n", out.MessageID, out.GroupSeq)
		if out.FanoutError != "" {
			fmt.Fprintf(os.Stderr, "warning: room post succeeded, but durable delivery fanout to members failed: %s\n", out.FanoutError)
		}
		return nil
	},
}

// ─── read ────────────────────────────────────────────────────────────────

var groupReadCmd = &cobra.Command{
	Use:   "read <group_urn>",
	Short: "Read group messages (non-destructive unless --mark-read)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group read: --as <urn> or $%s is required", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.ListMessages(cmdCtx(cmd), args[0], client.ListMessagesParams{
			As:       caller,
			SinceSeq: groupReadSinceSeq,
			ThreadID: groupReadThread,
		})
		if err != nil {
			return classifyErr(err)
		}
		if groupReadMarkRead && out.NextSeq > 0 {
			if err := gc.MarkRead(cmdCtx(cmd), args[0], caller, out.NextSeq); err != nil {
				return classifyErr(err)
			}
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		printGroupMessages(out.Messages)
		fmt.Printf("\nnext_seq: %d (use --since-seq %d to paginate)\n", out.NextSeq, out.NextSeq)
		return nil
	},
}

// ─── mentions ────────────────────────────────────────────────────────────

var groupMentionsCmd = &cobra.Command{
	Use:   "mentions",
	Short: "List your own mention notices across all groups",
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group mentions: --as <urn> or $%s is required", callerEnvKey)
		}
		var since time.Time
		if groupMentionsSince != "" {
			t, err := time.Parse(time.RFC3339, groupMentionsSince)
			if err != nil {
				return validationErr("group mentions: --since must be RFC3339: %v", err)
			}
			since = t
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.Mentions(cmdCtx(cmd), client.MentionsParams{
			As:    caller,
			Since: since,
			Limit: groupMentionsLimit,
		})
		if err != nil {
			return classifyErr(err)
		}
		if groupJSONFlag {
			return printJSON(out)
		}
		printGroupMessages(out)
		return nil
	},
}

// ─── archive ─────────────────────────────────────────────────────────────

var groupArchiveCmd = &cobra.Command{
	Use:   "archive <group_urn>",
	Short: "Archive a group (soft-delete; read-only after)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		caller := callerURN()
		if caller == "" {
			return validationErr("group archive: --as <urn> or $%s is required", callerEnvKey)
		}
		gc, err := groupsClient()
		if err != nil {
			return classifyErr(err)
		}
		out, err := gc.Archive(cmdCtx(cmd), args[0], caller)
		if err != nil {
			return classifyErr(err)
		}
		fmt.Printf("archived: %s status=%s\n", out.URN, out.Status)
		return nil
	},
}

// ─── pretty-printers ─────────────────────────────────────────────────────

// printGroupProfile emits a stable line-oriented rendering of a group
// Profile. Differs from printProfile (registry's) only in dropping
// agent-specific fields like skills/links — groups don't carry those.
func printGroupProfile(p registry.Profile) {
	fmt.Printf("urn:             %s\n", p.URN)
	fmt.Printf("kind:            %s\n", p.Kind)
	fmt.Printf("display_name:    %s\n", p.DisplayName)
	if p.Description != "" {
		fmt.Printf("description:     %s\n", p.Description)
	}
	if p.Role != "" {
		fmt.Printf("category:        %s\n", p.Role)
	}
	fmt.Printf("status:          %s\n", p.Status)
	fmt.Printf("created_at:      %s\n", p.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Printf("updated_at:      %s\n", p.UpdatedAt.UTC().Format(time.RFC3339))
	if len(p.Capabilities) > 0 {
		fmt.Printf("capabilities:    [%s]\n", strings.Join(p.Capabilities, ", "))
	}
}

// printGroupTable emits a tabwriter-aligned list of group profiles.
func printGroupTable(rows []registry.Profile) {
	if len(rows) == 0 {
		fmt.Println("(no groups)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "URN\tDISPLAY_NAME\tCATEGORY\tSTATUS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.URN, r.DisplayName, dashIfEmpty(r.Role), r.Status)
	}
	_ = tw.Flush()
}

// printGroupMessages emits a tabwriter-aligned list of GroupMessages.
// Payloads are truncated to the first 80 chars for a one-line
// rendering; --json gives the raw envelope.
func printGroupMessages(msgs []registry.GroupMessage) {
	if len(msgs) == 0 {
		fmt.Println("(no messages)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tFROM\tTHREAD\tCREATED_AT\tPAYLOAD")
	for _, m := range msgs {
		thread := dashIfEmpty(m.ThreadID)
		payloadPrev := truncate(string(m.Payload), 80)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			strconv.FormatInt(m.GroupSeq, 10),
			m.FromURN,
			thread,
			m.CreatedAt.UTC().Format(time.RFC3339),
			payloadPrev,
		)
	}
	_ = tw.Flush()
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// resetGroupFlags clears per-subcommand cobra flag state between tests.
// Mirrors resetRegistryFlags.
func resetGroupFlags() {
	groupAsFlag = ""
	groupJSONFlag = false
	groupCreateName = ""
	groupCreateDescription = ""
	groupCreateCategory = ""
	groupCreateCapability = nil
	groupListMine = false
	groupInviteRole = ""
	groupPostThread = ""
	groupReadSinceSeq = 0
	groupReadThread = ""
	groupReadMarkRead = false
	groupMentionsSince = ""
	groupMentionsLimit = 0
}

// Reference context.Background to keep imports tidy under build matrices.
var _ = context.Background

func init() {
	// Shared flags. Registered per-subcommand because cobra's
	// PersistentFlags() on the parent applies the flag globally, and we
	// want the JSON / as flags to surface only where they're used.
	for _, c := range []*cobra.Command{
		groupCreateCmd, groupListCmd, groupShowCmd, groupInviteCmd, groupKickCmd,
		groupLeaveCmd, groupPostCmd, groupReadCmd, groupMentionsCmd, groupArchiveCmd,
	} {
		c.Flags().StringVar(&groupAsFlag, "as", "",
			"caller URN (v060-05 auth surrogate; falls back to $"+callerEnvKey+")")
	}
	for _, c := range []*cobra.Command{
		groupCreateCmd, groupListCmd, groupShowCmd, groupReadCmd, groupMentionsCmd,
		groupInviteCmd, groupPostCmd,
	} {
		c.Flags().BoolVar(&groupJSONFlag, "json", false, "emit raw JSON instead of the pretty rendering")
	}

	// create.
	groupCreateCmd.Flags().StringVar(&groupCreateName, "name", "", "display name (required)")
	groupCreateCmd.Flags().StringVar(&groupCreateDescription, "description", "", "free-form description")
	groupCreateCmd.Flags().StringVar(&groupCreateCategory, "category", "", "group category (free-form, e.g. 'design-room')")
	groupCreateCmd.Flags().StringArrayVar(&groupCreateCapability, "capability", nil, "topic tag (repeatable)")

	// list.
	groupListCmd.Flags().BoolVar(&groupListMine, "mine", false, "list groups the caller belongs to (required v1)")

	// invite.
	groupInviteCmd.Flags().StringVar(&groupInviteRole, "role", "member", "role at invite time: member | moderator")

	// post.
	groupPostCmd.Flags().StringVar(&groupPostThread, "thread", "", "thread id for sub-conversation grouping")

	// read.
	groupReadCmd.Flags().Int64Var(&groupReadSinceSeq, "since-seq", 0, "lower bound on group_seq (exclusive); 0 = caller's last_read_seq")
	groupReadCmd.Flags().StringVar(&groupReadThread, "thread", "", "filter to one thread id")
	groupReadCmd.Flags().BoolVar(&groupReadMarkRead, "mark-read", false, "bump the read cursor to next_seq after listing")

	// mentions.
	groupMentionsCmd.Flags().StringVar(&groupMentionsSince, "since", "", "RFC3339 lower-bound timestamp")
	groupMentionsCmd.Flags().IntVar(&groupMentionsLimit, "limit", 0, "max mentions to return (0 = server default)")

	groupCmd.AddCommand(
		groupCreateCmd,
		groupListCmd,
		groupShowCmd,
		groupInviteCmd,
		groupKickCmd,
		groupLeaveCmd,
		groupPostCmd,
		groupReadCmd,
		groupMentionsCmd,
		groupArchiveCmd,
	)
	rootCmd.AddCommand(groupCmd)
}

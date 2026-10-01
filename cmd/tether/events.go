package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/client"
)

var (
	eventsJSONFlag bool
	eventsScopeArg []string
	eventsKindArg  []string
	eventsSession  string
	eventsSinceSeq int64
	eventsCursor   int64
	eventsLimit    int
)

var eventsCmd = &cobra.Command{
	Use:   "events",
	Short: "Event history commands",
}

var eventsHistoryCmd = &cobra.Command{
	Use:   "history",
	Short: "Query durable daemon/session/broker event history",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		out, err := c.EventsHistory(cmdCtx(cmd), client.EventsHistoryQuery{
			Scopes:    eventsScopeArg,
			Kinds:     eventsKindArg,
			SessionID: eventsSession,
			SinceSeq:  eventsSinceSeq,
			Cursor:    eventsCursor,
			Limit:     eventsLimit,
		})
		if err != nil {
			return classifyErr(err)
		}
		if eventsJSONFlag {
			return printJSON(map[string]any{"events": out.Events, "count": len(out.Events), "next_cursor": out.NextCursor})
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SEQ\tAT\tSCOPE\tSESSION\tKIND\tPAYLOAD")
		for _, ev := range out.Events {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", ev.Seq, ev.At, ev.Scope, ev.SessionID, ev.Kind, ev.PayloadJSON)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if out.NextCursor > 0 {
			fmt.Fprintf(os.Stdout, "next_cursor: %d\n", out.NextCursor)
		}
		return nil
	},
}

var eventsWatchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Stream live daemon/session/broker events",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return classifyErr(err)
		}
		stream, errCh, err := c.StreamEvents(cmdCtx(cmd), client.EventsStreamQuery{
			Scopes:    eventsScopeArg,
			Kinds:     eventsKindArg,
			SessionID: eventsSession,
			SinceSeq:  eventsSinceSeq,
		})
		if err != nil {
			return classifyErr(err)
		}
		for {
			select {
			case ev, ok := <-stream:
				if !ok {
					return nil
				}
				if eventsJSONFlag {
					if err := printJSON(map[string]any{
						"seq":          ev.Seq,
						"kind":         ev.Kind,
						"scope":        ev.Scope,
						"session_id":   ev.SessionID,
						"payload_json": ev.PayloadJSON,
					}); err != nil {
						return err
					}
					continue
				}
				fmt.Printf("%d\t%s\t%s\t%s\t%s\n", ev.Seq, ev.Scope, ev.SessionID, ev.Kind, ev.PayloadJSON)
			case err := <-errCh:
				if err != nil {
					return classifyErr(err)
				}
				errCh = nil
			case <-cmdCtx(cmd).Done():
				return nil
			}
		}
	},
}

func init() {
	eventsHistoryCmd.Flags().BoolVar(&eventsJSONFlag, "json", false, "print JSON output")
	eventsHistoryCmd.Flags().StringArrayVar(&eventsScopeArg, "scope", nil, "event scope filter; repeatable (daemon, session, broker)")
	eventsHistoryCmd.Flags().StringArrayVar(&eventsKindArg, "kind", nil, "event kind filter; repeatable")
	eventsHistoryCmd.Flags().StringVar(&eventsSession, "session-id", "", "exact session id filter")
	eventsHistoryCmd.Flags().Int64Var(&eventsSinceSeq, "since-seq", 0, "only return events with seq greater than this value")
	eventsHistoryCmd.Flags().Int64Var(&eventsCursor, "cursor", 0, "pagination cursor; return events with seq less than this value")
	eventsHistoryCmd.Flags().IntVar(&eventsLimit, "limit", 100, "max events to return")

	eventsWatchCmd.Flags().BoolVar(&eventsJSONFlag, "json", false, "print JSON output")
	eventsWatchCmd.Flags().StringArrayVar(&eventsScopeArg, "scope", nil, "event scope filter; repeatable (daemon, session, broker)")
	eventsWatchCmd.Flags().StringArrayVar(&eventsKindArg, "kind", nil, "event kind filter; repeatable")
	eventsWatchCmd.Flags().StringVar(&eventsSession, "session-id", "", "exact session id filter")
	eventsWatchCmd.Flags().Int64Var(&eventsSinceSeq, "since-seq", 0, "replay events with seq greater than this value before live streaming")

	eventsCmd.AddCommand(eventsHistoryCmd, eventsWatchCmd)
}

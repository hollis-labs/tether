package main

// workstream_digest.go — `mux workstreams digest`, the CLI end of the recovery
// and audit view.
//
// S5 of SP-20260912-0001 (CW-20260912-0063). Assembly is in
// internal/store/digest.go; the wire shape is internal/api/digest.go.

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

var (
	digestSession    string
	digestWorkstream string
	digestForRef     string
	digestKind       string
	digestRelation   string
	digestSource     string
	digestSince      string
	digestLimit      int
	digestLeftOnly   bool
	digestJSON       bool
)

var workstreamDigestCmd = &cobra.Command{
	Use:   "digest",
	Short: "What a session or workstream touched, and what it left behind",
	Long: `Assemble the recovery view for one session or a whole workstream.

Refs are split into two sections. LEFT BEHIND is created and updated — what
this work produced, and what a reviewer or a recovery instruction cares about.
TOUCHED is read and referenced — what it consulted. Collapsing the two into a
single "touched 14 tasks" is the thing this command exists not to do.

Three grains of entry:

  --session <id>       this session's own refs (the everyday view)
  --workstream <id>    the roll-up across every session in the container,
                       which is what survives a compaction
  --for-ref <k>:<id>   resolve which workstreams touched an object, then digest
                       each one. Prints ALL matches — two efforts touching the
                       same task is ordinary, and picking one would look
                       authoritative while being wrong.

BEFORE READING AN EMPTY DIGEST AS "NOTHING HAPPENED", read the coverage block.
Each session reports whether its proxy could produce an observed ref at all; if
none could, an absent proxy column is a fact about configuration rather than
about the work.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		set := 0
		for _, v := range []string{digestSession, digestWorkstream, digestForRef} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			return validationErr("workstreams digest: pass exactly one of --session, --workstream or --for-ref")
		}
		c, err := registryClientFactory()
		if err != nil {
			return classifyErr(err)
		}
		q := client.DigestQuery{
			Kind: digestKind, Relation: digestRelation, Source: digestSource,
			Since: digestSince, Limit: digestLimit,
		}
		ctx := cmdCtx(cmd)

		switch {
		case digestSession != "":
			out, err := c.SessionDigest(ctx, digestSession, q)
			if err != nil {
				return classifyErr(err)
			}
			return emitDigest(out)
		case digestWorkstream != "":
			out, err := c.WorkstreamDigest(ctx, digestWorkstream, q)
			if err != nil {
				return classifyErr(err)
			}
			return emitDigest(out)
		}

		matches, err := c.WorkstreamsForRef(ctx, digestForRef)
		if err != nil {
			return classifyErr(err)
		}
		if len(matches) == 0 {
			// Said explicitly rather than printing nothing: an empty result
			// here has two readings -- no workstream touched it, or nothing
			// recorded the touch -- and silence would let a reader pick the
			// first without being told the second exists.
			fmt.Printf("no workstream contains a session that touched %s\n", digestForRef)
			fmt.Println("(a ref is only recorded when something attached it; absence is not evidence the object was untouched)")
			return nil
		}
		// The count leads, because "which workstream" answered with three is a
		// materially different answer from one and the reader must see that
		// before the first digest scrolls past.
		fmt.Printf("%s matched %d workstream(s)\n\n", digestForRef, len(matches))
		for i, m := range matches {
			out, err := c.WorkstreamDigest(ctx, m.ID, q)
			if err != nil {
				return classifyErr(err)
			}
			if i > 0 {
				fmt.Println()
			}
			if err := emitDigest(out); err != nil {
				return err
			}
		}
		return nil
	},
}

func emitDigest(d api.DigestResponse) error {
	if digestJSON {
		return printJSON(d)
	}
	renderDigest(os.Stdout, d, digestLeftOnly)
	return nil
}

// renderDigest writes the human form. Writer-based rather than printing
// straight to stdout so the worked example in docs/workstreams.md is generated
// from this exact function rather than hand-authored -- a documented sample
// that nothing produces is fabricated evidence, and it drifts silently.
func renderDigest(w io.Writer, d api.DigestResponse, leftOnly bool) {
	switch {
	case d.Grain == "session":
		fmt.Fprintf(w, "session %s\n", d.SessionID)
		if d.Workstream != nil {
			fmt.Fprintf(w, "  in workstream %s%s\n", d.Workstream.ID, labelSuffix(d.Workstream.Name))
			fmt.Fprintf(w, "  (roll-up across the lineage: mux workstreams digest --workstream %s)\n", d.Workstream.ID)
		} else {
			fmt.Fprintln(w, "  in no workstream — nothing to roll up to")
		}
	case d.Workstream != nil:
		fmt.Fprintf(w, "workstream %s%s\n", d.Workstream.ID, labelSuffix(d.Workstream.Name))
		fmt.Fprintf(w, "  status %s", d.Workstream.Status)
		if d.Workstream.WorkflowID != "" {
			fmt.Fprintf(w, "  workflow %s", d.Workstream.WorkflowID)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "\nSPAN  %d session(s), ", d.Span.SessionCount)
	if d.Span.SpansLineage {
		fmt.Fprintln(w, "crossing a lineage boundary")
	} else {
		// Spelled out, because at session count 1 the two facts look the same
		// and only one of them says the container did its job.
		fmt.Fprintln(w, "no lineage boundary crossed")
	}
	for _, s := range d.Span.Sessions {
		parent := ""
		if s.ParentSessionID != "" {
			parent = "  <- " + s.ParentSessionID
		}
		fmt.Fprintf(w, "  %s  %-11s %-10s refs=%-4d attribution=%s%s\n",
			s.ID, s.Intent, s.State, s.RefCount, s.RefAttribution, parent)
	}

	printDigestSection(w, "LEFT BEHIND (created / updated)", d.LeftBehind)
	if !leftOnly {
		printDigestSection(w, "TOUCHED (read / referenced)", d.Touched)
	}

	fmt.Fprintf(w, "\nTOTALS  %d ref(s)", d.Totals.Refs)
	if len(d.Totals.BySource) > 0 {
		fmt.Fprintf(w, "  by source: %s", countsLine(d.Totals.BySource))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "COVERAGE  limit=%d truncated=%v  attribution: %s\n",
		d.Coverage.Limit, d.Coverage.Truncated, countsLine(d.Coverage.Attribution))
	if d.Coverage.Truncated {
		fmt.Fprintln(w, "  ! the ref list was cut at the limit; raise --limit to see the rest")
	}
	if d.Coverage.Note != "" {
		fmt.Fprintf(w, "  ! %s\n", d.Coverage.Note)
	}
}

func printDigestSection(w io.Writer, title string, groups []api.DigestKindGroupDTO) {
	fmt.Fprintf(w, "\n%s\n", title)
	if len(groups) == 0 {
		fmt.Fprintln(w, "  (none)")
		return
	}
	for _, g := range groups {
		fmt.Fprintf(w, "  %s\n", g.Kind)
		printDigestRefs(w, "created", g.Created)
		printDigestRefs(w, "updated", g.Updated)
		printDigestRefs(w, "read", g.Read)
		printDigestRefs(w, "referenced", g.Referenced)
	}
}

func printDigestRefs(w io.Writer, relation string, refs []api.DigestRefDTO) {
	for _, r := range refs {
		// source is on every line rather than summarized, because the whole
		// point of the column is telling an observation from an assertion, and
		// that distinction is per-ref.
		fmt.Fprintf(w, "    %-11s %-28s %-6s %s\n", relation, r.RefID, r.Source, r.At)
	}
}

func countsLine(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	// Fixed key order rather than map iteration: a digest a human re-runs
	// should not reshuffle between calls. Unknown keys follow, sorted, so a
	// value added later still prints instead of vanishing.
	known := []string{"proxy", "api", "agent", "none", "unlaunched", "unknown"}
	seen := map[string]bool{}
	parts := []string{}
	for _, k := range known {
		if n, ok := m[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range m {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func labelSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

func init() {
	f := workstreamDigestCmd.Flags()
	f.StringVar(&digestSession, "session", "", "digest one session's own refs")
	f.StringVar(&digestWorkstream, "workstream", "", "digest the roll-up across a workstream's sessions")
	f.StringVar(&digestForRef, "for-ref", "", "resolve <kind>:<ref_id> to its workstreams and digest each (prints all matches)")
	f.StringVar(&digestKind, "kind", "", "filter to one ref kind")
	f.StringVar(&digestRelation, "relation", "", "filter to one relation: created, updated, read, referenced")
	f.StringVar(&digestSource, "source", "", "filter to one source: proxy, api, agent")
	f.StringVar(&digestSince, "since", "", "RFC3339 UTC lower bound: what was in flight, not everything ever")
	f.IntVar(&digestLimit, "limit", 0, "maximum refs; the output reports whether it truncated")
	f.BoolVar(&digestLeftOnly, "left-behind", false, "print only the created/updated section")
	f.BoolVar(&digestJSON, "json", false, "print JSON")

	workstreamsCmd.AddCommand(workstreamDigestCmd)
}

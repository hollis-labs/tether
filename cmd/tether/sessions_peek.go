package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

const (
	peekDefaultLines = 40
	peekArgLimit     = 160
	peekTextLimit    = 300
)

// peekSecretRE masks token-looking values and KEY=value pairs for secret-ish
// names before anything is printed.
var peekSecretRE = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{8,}|gh[pousr]_[A-Za-z0-9]{16,}|xox[abp]-[A-Za-z0-9\-]{10,}|bearer\s+[A-Za-z0-9._\-]{12,}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]*)`)
var peekEnvRE = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|CREDENTIAL)[A-Z0-9_]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)

func peekRedact(s string) string {
	s = peekEnvRE.ReplaceAllString(s, "${1}${2}[redacted]")
	return peekSecretRE.ReplaceAllString(s, "[redacted]")
}

func peekTrunc(s string, n int) string {
	s = strings.Join(strings.Fields(peekRedact(s)), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// peekDigest renders the Claude stream-json lines in data as one digest line
// each and returns the last n of them. Non-JSON lines are skipped.
func peekDigest(data []byte, n int) []string {
	var out []string
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type       string `json:"type"`
			Subtype    string `json:"subtype"`
			Model      string `json:"model"`
			MCPServers []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"mcp_servers"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Result  string  `json:"result"`
			Cost    float64 `json:"total_cost_usd"`
			Turns   int     `json:"num_turns"`
			IsError bool    `json:"is_error"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "system":
			if ev.Subtype != "init" {
				continue
			}
			var mcp []string
			for _, m := range ev.MCPServers {
				mcp = append(mcp, m.Name+"="+m.Status)
			}
			out = append(out, fmt.Sprintf("init   model=%s mcp=[%s]", ev.Model, strings.Join(mcp, " ")))
		case "assistant", "user":
			var blocks []struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Name    string          `json:"name"`
				Input   json.RawMessage `json:"input"`
				Content json.RawMessage `json:"content"`
				IsError bool            `json:"is_error"`
			}
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if ev.Type == "assistant" {
						out = append(out, "say    "+peekTrunc(b.Text, peekTextLimit))
					}
				case "tool_use":
					out = append(out, fmt.Sprintf("tool   %s %s", b.Name, peekTrunc(string(b.Input), peekArgLimit)))
				case "tool_result":
					tag := "result"
					if b.IsError {
						tag = "error "
					}
					out = append(out, tag+" "+peekTrunc(peekContentText(b.Content), peekArgLimit))
				}
			}
		case "result":
			status := "ok"
			if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
				status = "error"
				if ev.Subtype != "" {
					status += ":" + ev.Subtype
				}
			}
			out = append(out, fmt.Sprintf("FINAL  %s turns=%d cost=$%.4f %s", status, ev.Turns, ev.Cost, peekTrunc(ev.Result, peekTextLimit)))
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// peekContentText flattens a tool_result content (string or block list).
func peekContentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			parts = append(parts, b.Text)
		}
		return strings.Join(parts, " ")
	}
	return string(raw)
}

func runSessionsPeek(ctx context.Context, w io.Writer, c *client.Client, id string, n int) error {
	sess, err := c.GetSession(ctx, id)
	if err != nil {
		return err
	}
	exit := "-"
	if sess.ExitCode != nil {
		exit = strconv.Itoa(*sess.ExitCode)
	}
	fmt.Fprintf(w, "session %s state=%s exit=%s provider=%s updated=%s\n", sess.ID, sess.State, exit, sess.ProviderID, sess.UpdatedAt)
	res, err := c.SessionLog(ctx, id, client.SessionLogOptions{Limit: api.MaxSessionLogBytes})
	if err != nil {
		return err
	}
	for _, l := range peekDigest(res.Data, n) {
		fmt.Fprintln(w, l)
	}
	return nil
}

var sessionsPeekCmd = &cobra.Command{
	Use:   "peek <id> [n]",
	Short: "Digest of a session's recent stream-json output (state, init, tool calls, results, final) with secrets redacted; works with --target",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		n := peekDefaultLines
		if len(args) == 2 {
			v, err := strconv.Atoi(args[1])
			if err != nil || v < 1 {
				return fmt.Errorf("n must be a positive integer, got %q", args[1])
			}
			n = v
		}
		c, err := newDaemonClient(catalogPath)
		if err != nil {
			return err
		}
		cmd.SilenceUsage = true
		return runSessionsPeek(cmd.Context(), cmd.OutOrStdout(), c, args[0], n)
	},
}

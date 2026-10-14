// Package sessionpeek renders a Claude stream-json session log as a short,
// secret-redacted digest. It backs `tether sessions peek` and tether_session_peek.
package sessionpeek

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	DefaultLines = 40
	argLimit     = 160
	textLimit    = 300
)

// secretRE masks token-looking values and KEY=value pairs for secret-ish
// names before anything is printed.
var secretRE = regexp.MustCompile(`(?i)(sk-[A-Za-z0-9_\-]{8,}|gh[pousr]_[A-Za-z0-9]{16,}|xox[abp]-[A-Za-z0-9\-]{10,}|bearer\s+[A-Za-z0-9._\-]{12,}|eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]*)`)
var envRE = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API_?KEY|CREDENTIAL)[A-Z0-9_]*)(\s*[=:]\s*)("[^"]*"|'[^']*'|\S+)`)

func redact(s string) string {
	s = envRE.ReplaceAllString(s, "${1}${2}[redacted]")
	return secretRE.ReplaceAllString(s, "[redacted]")
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(redact(s)), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Digest renders the Claude stream-json lines in data as one digest line
// each and returns the last n of them. Non-JSON lines are skipped.
func Digest(data []byte, n int) []string {
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
						out = append(out, "say    "+trunc(b.Text, textLimit))
					}
				case "tool_use":
					out = append(out, fmt.Sprintf("tool   %s %s", b.Name, trunc(string(b.Input), argLimit)))
				case "tool_result":
					tag := "result"
					if b.IsError {
						tag = "error "
					}
					out = append(out, tag+" "+trunc(contentText(b.Content), argLimit))
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
			out = append(out, fmt.Sprintf("FINAL  %s turns=%d cost=$%.4f %s", status, ev.Turns, ev.Cost, trunc(ev.Result, textLimit)))
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// contentText flattens a tool_result content (string or block list).
func contentText(raw json.RawMessage) string {
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

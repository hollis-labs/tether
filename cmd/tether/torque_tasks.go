package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
)

var unsafeTaskPathRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func buildTorqueTaskLaunchAugment(ctx context.Context, baseURL string, taskIDs []string, taskDir string) (config.LaunchInjection, string, error) {
	baseURL = resolveTorqueBaseURL(baseURL)
	var err error
	taskDir, err = cleanTaskBundleDir(taskDir)
	if err != nil {
		return config.LaunchInjection{}, "", err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	type taskRef struct {
		id     string
		safeID string
	}
	refs := make([]taskRef, 0, len(taskIDs))
	seenIDs := map[string]struct{}{}
	seenSafeIDs := map[string]string{}
	for _, id := range taskIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seenIDs[id]; ok {
			return config.LaunchInjection{}, "", fmt.Errorf("duplicate Torque task id %q", id)
		}
		seenIDs[id] = struct{}{}
		safeID := safeTaskDirName(id)
		if prior, ok := seenSafeIDs[safeID]; ok {
			return config.LaunchInjection{}, "", fmt.Errorf("torque task ids %q and %q both map to bundle directory %q", prior, id, safeID)
		}
		seenSafeIDs[safeID] = id
		refs = append(refs, taskRef{id: id, safeID: safeID})
	}
	tasks := make([]torqueTaskBundle, 0, len(refs))
	for _, ref := range refs {
		task, err := fetchTorqueTask(ctx, client, baseURL, ref.id)
		if err != nil {
			return config.LaunchInjection{}, "", err
		}
		tasks = append(tasks, torqueTaskBundle{ID: ref.id, SafeID: ref.safeID, Task: task})
	}
	if len(tasks) == 0 {
		return config.LaunchInjection{}, "", fmt.Errorf("at least one --torque-task id is required")
	}

	files := []config.InjectedFile{{
		Kind:    "raw",
		RelPath: filepath.ToSlash(filepath.Join(taskDir, "README.md")),
		Content: renderTorqueTasksReadme(baseURL, tasks),
	}}
	for _, task := range tasks {
		dir := filepath.ToSlash(filepath.Join(taskDir, task.SafeID))
		taskJSON, err := json.MarshalIndent(task.Task, "", "  ")
		if err != nil {
			return config.LaunchInjection{}, "", fmt.Errorf("marshal Torque task %s: %w", task.ID, err)
		}
		files = append(files,
			config.InjectedFile{Kind: "raw", RelPath: dir + "/task.json", Content: string(taskJSON) + "\n"},
			config.InjectedFile{Kind: "raw", RelPath: dir + "/task.md", Content: renderTorqueTaskMarkdown(task)},
			config.InjectedFile{Kind: "raw", RelPath: dir + "/process.md", Content: renderTorqueTaskProcess(baseURL, task.ID)},
		)
	}

	return config.LaunchInjection{NativeFiles: files}, renderTorqueTaskPromptAppend(taskDir, tasks), nil
}

type torqueTaskBundle struct {
	ID     string
	SafeID string
	Task   map[string]any
}

func resolveTorqueBaseURL(in string) string {
	if in == "" {
		in = os.Getenv("TORQUE_BASE_URL")
	}
	if in == "" {
		in = "http://127.0.0.1:8990"
	}
	return strings.TrimRight(in, "/")
}

func cleanTaskBundleDir(in string) (string, error) {
	clean := filepath.ToSlash(filepath.Clean(strings.TrimSpace(in)))
	if clean == "." {
		return "", fmt.Errorf("torque task dir must not be empty")
	}
	if strings.HasPrefix(clean, "~") || filepath.IsAbs(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("torque task dir must be a safe bootdir-relative path")
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return "", fmt.Errorf("torque task dir must be a safe bootdir-relative path")
		}
	}
	return clean, nil
}

func fetchTorqueTask(ctx context.Context, client *http.Client, baseURL, taskID string) (map[string]any, error) {
	endpoint := baseURL + "/api/v1/tasks/" + url.PathEscape(taskID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build Torque task request %s: %w", taskID, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Torque task %s from %s: %w", taskID, endpoint, err)
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read Torque task %s response: %w", taskID, readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch Torque task %s: HTTP %d: %s", taskID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var task map[string]any
	if err := json.Unmarshal(body, &task); err != nil {
		return nil, fmt.Errorf("parse Torque task %s response: %w", taskID, err)
	}
	if task["id"] == nil {
		task["id"] = taskID
	}
	return task, nil
}

func safeTaskDirName(id string) string {
	s := unsafeTaskPathRE.ReplaceAllString(id, "_")
	s = strings.Trim(s, "._-")
	if s == "" {
		return "task"
	}
	return s
}

func renderTorqueTasksReadme(baseURL string, tasks []torqueTaskBundle) string {
	var b strings.Builder
	b.WriteString("# Torque Task Bundle\n\n")
	b.WriteString("This bundle was planted at launch time from Torque's HTTP API. Treat these files as the task handoff source of truth for this run.\n\n")
	b.WriteString("- Do the requested implementation work directly in the workspace.\n")
	b.WriteString("- Avoid Torque MCP tools, memory tools, or task discovery unless a task file explicitly tells you otherwise.\n")
	b.WriteString("- Each task folder contains `task.md`, `task.json`, and `process.md`.\n")
	b.WriteString("- When the work is complete or blocked, run the exact command in that task's `process.md`.\n\n")
	b.WriteString("Torque API base URL captured for this launch: `" + baseURL + "`\n\n")
	b.WriteString("## Tasks\n\n")
	for _, task := range tasks {
		b.WriteString("- [" + task.ID + "](" + task.SafeID + "/task.md) - " + stringField(task.Task, "title") + "\n")
	}
	b.WriteString("\n")
	return b.String()
}

func renderTorqueTaskMarkdown(task torqueTaskBundle) string {
	var b strings.Builder
	b.WriteString("# Torque Task " + task.ID + "\n\n")
	writeField := func(label, key string) {
		if v := stringField(task.Task, key); v != "" {
			b.WriteString("- " + label + ": `" + v + "`\n")
		}
	}
	writeField("Title", "title")
	writeField("Status", "status")
	writeField("Priority", "priority")
	writeField("Project", "project_id")
	writeField("Sprint", "sprint_id")
	writeField("Working dir", "working_dir")
	writeField("Agent profile", "agent_profile")
	writeField("Executor", "executor")
	b.WriteString("\n## Instructions\n\n")
	b.WriteString("Do the work described below. Do not browse Torque for extra context unless the description asks for it. Use `process.md` when you are ready to report the outcome back to Torque.\n\n")
	if desc := stringField(task.Task, "description"); desc != "" {
		b.WriteString("## Description\n\n")
		b.WriteString(strings.TrimSpace(desc))
		b.WriteString("\n\n")
	}
	if subs, ok := task.Task["subtodos"].([]any); ok && len(subs) > 0 {
		b.WriteString("## Subtodos\n\n")
		for _, raw := range subs {
			if m, ok := raw.(map[string]any); ok {
				title := stringField(m, "title")
				if title == "" {
					title = stringField(m, "text")
				}
				if title != "" {
					box := " "
					if done, _ := m["done"].(bool); done {
						box = "x"
					}
					fmt.Fprintf(&b, "- [%s] %s\n", box, title)
				}
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func renderTorqueTaskProcess(baseURL, taskID string) string {
	taskURL := baseURL + "/api/v1/tasks/" + url.PathEscape(taskID)
	var b strings.Builder
	b.WriteString("# Torque Process Commands\n\n")
	b.WriteString("Run exactly one of these from any shell when the task outcome is known.\n\n")
	b.WriteString("## Ready For Review\n\n")
	b.WriteString("```sh\n")
	b.WriteString("curl -sS -X POST '" + taskURL + "/transition' -H 'content-type: application/json' -d '{\"status\":\"review\"}'\n")
	b.WriteString("```\n\n")
	b.WriteString("## Blocked\n\n")
	b.WriteString("If you are blocked, first set the blocked reason, then transition the task:\n\n")
	b.WriteString("```sh\n")
	b.WriteString("curl -sS -X PUT '" + taskURL + "' -H 'content-type: application/json' -d '{\"blocked_reason\":\"BLOCKED: replace this sentence with the concrete blocker\"}'\n")
	b.WriteString("curl -sS -X POST '" + taskURL + "/transition' -H 'content-type: application/json' -d '{\"status\":\"blocked\"}'\n")
	b.WriteString("```\n")
	return b.String()
}

func renderTorqueTaskPromptAppend(taskDir string, tasks []torqueTaskBundle) string {
	var b strings.Builder
	b.WriteString("# Planted Torque Tasks\n\n")
	b.WriteString("A launch-time Torque task bundle is available under `" + taskDir + "/` in the planted boot directory. Start with `" + taskDir + "/README.md` when reading relative to the boot directory, or `$CODEX_HOME/" + taskDir + "/README.md` from Codex app-server shell/tool commands. Then complete the listed task files. Avoid Torque MCP/tool discovery during this run unless the task bundle explicitly instructs it; the needed task context and exact lifecycle commands are already planted.\n\n")
	b.WriteString("Task IDs: ")
	for i, task := range tasks {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("`" + task.ID + "`")
	}
	b.WriteString("\n")
	return b.String()
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(raw)
	}
}

func mergeLaunchInjectionJSON(existing string, extra config.LaunchInjection) (string, error) {
	var merged config.LaunchInjection
	if strings.TrimSpace(existing) != "" {
		if err := json.Unmarshal([]byte(existing), &merged); err != nil {
			return "", fmt.Errorf("injection: parse existing: %w", err)
		}
	}
	merged.NativeFiles = append(merged.NativeFiles, extra.NativeFiles...)
	merged.BootDirOverlay = append(merged.BootDirOverlay, extra.BootDirOverlay...)
	if len(merged.NativeFiles) == 0 && len(merged.BootDirOverlay) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("injection: marshal merged: %w", err)
	}
	return string(raw), nil
}

func appendPromptText(base, extra string) string {
	if strings.TrimSpace(extra) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	var b bytes.Buffer
	b.WriteString(base)
	if !strings.HasSuffix(base, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(extra)
	return b.String()
}

// Package correlationregistry writes secret-free launch observations to the
// agency Registry, independently of Tether's federation directory.
package correlationregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hollis-labs/libs/security/secrets/credfile"
)

// Config is opt-in daemon configuration. Credentials never enter launch env.
type Config struct {
	URL       string `yaml:"url,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
}

type Reference struct {
	Authority string `json:"authority"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
}

// Observation contains only observed identifiers, never a signed run token or
// a copy of arbitrary launch environment/configuration.
type Observation struct {
	URN               string `json:"urn,omitempty"`
	Session           string `json:"session"`
	RunID             string `json:"run_id"`
	DispatchRunID     string `json:"dispatch_run_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	WorkstreamID      string `json:"workstream_id,omitempty"`
	JTI               string `json:"jti,omitempty"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
}

type Payload struct {
	Schema   string               `json:"schema"`
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Links    map[string]Reference `json:"links"`
	Metadata Observation          `json:"metadata"`
}

func (o Observation) Payload() Payload {
	links := map[string]Reference{"session": {Authority: "tether", Kind: "session", ID: o.Session}}
	if strings.HasPrefix(o.URN, "msg://agent/") {
		links["agent_urn"] = Reference{Authority: "tether", Kind: "agent", ID: o.URN}
	}
	if o.TaskID != "" {
		links["task"] = Reference{Authority: "torque", Kind: "task", ID: o.TaskID}
	}
	// Legacy local IDs are observations only, not Registry references.
	if strings.HasPrefix(o.WorkstreamID, "workstream:") {
		links["workstream"] = Reference{Authority: "registry", Kind: "workstream", ID: o.WorkstreamID}
	}
	return Payload{Schema: "correlation/1", ID: o.RunID, Name: "Tether execution " + o.RunID, Links: links, Metadata: o}
}

// Register makes one bounded generic record_create request. Exact replays use
// the execution ID as their receipt key. Response bodies never enter logs.
func Register(ctx context.Context, cfg Config, roots []string, o Observation) error {
	if o.Session == "" || o.RunID == "" {
		return errors.New("correlation identifiers missing")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid correlation Registry URL")
	}
	if cfg.TokenFile == "" {
		return errors.New("correlation Registry token_file required")
	}
	if home, e := os.UserHomeDir(); e == nil {
		roots = append(append([]string(nil), roots...), home)
	}
	token, err := credfile.Read(cfg.TokenFile, credfile.Options{Roots: roots})
	if err != nil {
		return errors.New("correlation Registry credential unavailable")
	}
	body := struct {
		Record struct {
			Schema  string  `json:"schema"`
			Key     string  `json:"key"`
			Payload Payload `json:"payload"`
		} `json:"record"`
		IdempotencyKey string `json:"idempotency_key"`
	}{}
	body.Record.Schema, body.Record.Key, body.Record.Payload = "correlation/1", "tether-execution-"+o.RunID, o.Payload()
	body.IdempotencyKey = "tether-execution-" + o.RunID
	raw, err := json.Marshal(body)
	if err != nil {
		return errors.New("correlation encoding failed")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/records"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(raw))
	if err != nil {
		return errors.New("correlation request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("correlation Registry unavailable")
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("correlation Registry HTTP %d", resp.StatusCode)
	}
	return nil
}

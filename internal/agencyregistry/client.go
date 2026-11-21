// Package agencyregistry implements the launcher's narrow outbound Registry port.
// It is separate from internal/registry, Tether's federation directory.
package agencyregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hollis-labs/libs/security/secrets/credfile"
)

type Config struct {
	URL            string `yaml:"url,omitempty" json:"url,omitempty"`
	CredentialFile string `yaml:"credential_file,omitempty" json:"credential_file,omitempty"`
}

// Facts are immutable launch observations. Never include credentials or tokens.
type Facts struct {
	ExportReceiptID   string `json:"export_receipt_id,omitempty"`
	ActivitySHA256    string `json:"activity_sha256,omitempty"`
	URN               string `json:"urn"`
	Session           string `json:"session"`
	RunID             string `json:"run_id"`
	TaskID            string `json:"task_id,omitempty"`
	WorkstreamID      string `json:"workstream_id,omitempty"`
	TokenJTI          string `json:"token_jti,omitempty"`
	ManifestDigest    string `json:"manifest_digest,omitempty"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	Traceparent       string `json:"traceparent,omitempty"`
}

// Register makes one bounded attempt. Only transient failures are retryable.
// Errors intentionally contain neither remote bodies nor credentials/URLs.
func Register(ctx context.Context, cfg Config, key string, facts Facts) (retry bool, err error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return false, fmt.Errorf("invalid Registry URL")
	}
	home, _ := os.UserHomeDir()
	credential, err := credfile.Read(cfg.CredentialFile, credfile.Options{Roots: []string{home}})
	if err != nil {
		return false, fmt.Errorf("registry credential file unavailable")
	}
	body, err := json.Marshal(facts)
	if err != nil {
		return false, fmt.Errorf("invalid Registry facts")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/correlations/register"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("invalid Registry request")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return true, fmt.Errorf("registry transport unavailable")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	return resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("registry status %d", resp.StatusCode)
}

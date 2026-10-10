package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/spf13/cobra"
)

type deviceAdminClient struct {
	client         *http.Client
	baseURL, token string
}
type deviceAdminFactory func() (*deviceAdminClient, error)

func localDeviceAdminClient() (*deviceAdminClient, error) {
	cfg, err := loadDaemonConfig(expandCatalogPath())
	if err != nil {
		return nil, err
	}
	// Refuse TCP before resolving credentials: the operator credential stays
	// on the local Unix transport even if the catalog selects a remote daemon.
	if !strings.HasPrefix(cfg.ListenAddr, "unix:") {
		return nil, fmt.Errorf("device administration requires the local Unix socket")
	}
	token, err := callerToken()
	if err != nil {
		return nil, fmt.Errorf("load local operator credential: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("local operator credential required")
	}
	client := daemon.DialHTTPClient(cfg.ListenAddr)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &deviceAdminClient{client: client, baseURL: daemon.BaseURL(cfg.ListenAddr), token: token}, nil
}

func (c *deviceAdminClient) call(ctx context.Context, method, path string, body, out any) error {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("local device administration unavailable")
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("local device administration refused (HTTP %d)", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return fmt.Errorf("invalid device administration response")
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("invalid device administration response")
		}
	}
	return nil
}

func newPairCommand(factory deviceAdminFactory) *cobra.Command {
	var scopes []string
	var ttl time.Duration
	var label, thumbprint string
	var asJSON bool
	cmd := &cobra.Command{Use: "pair", Short: "Create a one-use pairing grant through the local operator socket", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := identity.NormalizeDeviceScopes(scopes); err != nil {
				return err
			}
			if ttl <= 0 || ttl > identity.MaxGrantTTL {
				return fmt.Errorf("ttl must be positive and at most one hour")
			}
			client, err := factory()
			if err != nil {
				return err
			}
			var grant identity.IssuedGrant
			if err := client.call(cmd.Context(), http.MethodPost, "/auth/pair", api.PairRequest{Label: label, Scopes: scopes, TTL: ttl.String(), KeyThumbprint: thumbprint}, &grant); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(grant)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), grant.Code)
			return err
		},
	}
	cmd.Flags().StringSliceVar(&scopes, "scope", []string{identity.ScopeRead}, "independent device scopes: read, operate, terminal, maintain, admin")
	cmd.Flags().DurationVar(&ttl, "ttl", identity.DefaultGrantTTL, "pairing grant lifetime (maximum one hour)")
	cmd.Flags().StringVar(&label, "label", "", "device label")
	cmd.Flags().StringVar(&thumbprint, "key-thumbprint", "", "reserved key binding (no DPoP verification)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print grant metadata and the one-time code as JSON")
	return cmd
}

func newAuthCommand(factory deviceAdminFactory) *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "List and revoke paired devices through the local operator socket"}
	cmd.AddCommand(&cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := factory()
		if err != nil {
			return err
		}
		var result struct {
			Devices []identity.Device `json:"devices"`
		}
		if err := client.call(cmd.Context(), http.MethodGet, "/auth/devices", nil, &result); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}})
	cmd.AddCommand(&cobra.Command{Use: "revoke <device-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := factory()
		if err != nil {
			return err
		}
		if err := client.call(cmd.Context(), http.MethodPost, "/auth/revoke", api.DeviceRevokeRequest{ID: args[0]}, nil); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Device revoked.")
		return err
	}})
	return cmd
}

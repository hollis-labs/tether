// Package devicepair exchanges a one-time hub grant without SSH enrollment.
package devicepair

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"strings"
)

func Exchange(ctx context.Context, client *http.Client, hub, code string) (identity.DeviceExchange, error) {
	var out identity.DeviceExchange
	body, _ := json.Marshal(map[string]string{"code": code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hub, "/")+identity.PairingExchangePath, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(environment.ProtocolHeader, "1")
	res, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return out, fmt.Errorf("pairing exchange HTTP %d", res.StatusCode)
	}
	if err = json.NewDecoder(res.Body).Decode(&out); err != nil {
		return out, err
	}
	if !identity.HasDeviceScope(out.Principal, identity.ScopeWorker) {
		return out, fmt.Errorf("pairing grant lacks worker scope")
	}
	return out, nil
}

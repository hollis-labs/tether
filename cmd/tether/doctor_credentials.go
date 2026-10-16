package main

import (
	"context"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
)

func checkCredentialHealth(cat *config.Catalog) []checkResult {
	if cat == nil {
		return []checkResult{warn("device-renewal", "runtime health unavailable", "check daemon/catalog")}
	}
	cfg, err := daemonConfigFromCatalog(cat)
	if err != nil {
		return []checkResult{warn("device-renewal", "runtime health unavailable", "check daemon/catalog")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.New(cfg.ListenAddr, client.WithToken("")).Health(ctx)
	if err != nil {
		return []checkResult{warn("device-renewal", "runtime health unavailable", "check daemon status")}
	}
	return credentialHealthChecks(h)
}
func credentialHealthChecks(h daemon.Health) []checkResult {
	out := []checkResult{}
	if h.Identity == nil || h.Identity.DeviceExpiry == nil {
		out = append(out, warn("device-renewal", "device expiry not reported", "check daemon version/identity configuration"))
	} else {
		d := h.Identity.DeviceExpiry
		switch {
		case d.Unavailable:
			out = append(out, warn("device-renewal", "device expiry lookup unavailable", "check identity store"))
		case d.Expiring > 0 || d.Expired > 0:
			out = append(out, warn("device-renewal", fmt.Sprintf("expiring within 7d=%d expired=%d", d.Expiring, d.Expired), "renew before expiry; expired credentials require pairing again"))
		default:
			out = append(out, ok("device-renewal", "no non-revoked device expires within 7d"))
		}
	}
	d := h.DenyListWatch
	if d == nil {
		return append(out, ok("deny-list-watch", "not configured on this daemon"))
	}
	if d.Unavailable || d.Stale || d.Backlog {
		return append(out, warn("deny-list-watch", "detector status unavailable, stale or catching up", "check the file detector timer and state path"))
	}
	if d.GapCount > d.AcknowledgedGapCount {
		return append(out, warn("deny-list-watch", "log rotation/truncation left an observation gap", "inspect the rotated log and verify polling/revocation, then acknowledge the detector"))
	}
	if d.WarningCount > d.AcknowledgedCount {
		return append(out, warn("deny-list-watch", fmt.Sprintf("unacknowledged poll failures=%d", d.WarningCount-d.AcknowledgedCount), "restore deny-list polling, verify revocation, then acknowledge the detector; alert delivery is separate"))
	}
	return append(out, ok("deny-list-watch", "no unacknowledged poll failures"))
}

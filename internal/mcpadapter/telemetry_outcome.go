package mcpadapter

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/telemetry"
)

func recordTargetError(ctx context.Context, err error) {
	var target *mcpgateway.TargetError
	if errors.As(err, &target) {
		class := events.ToolErrorDenied
		if target.Unavailable {
			class = events.ToolErrorUpstreamDown
		}
		telemetry.SetErrorClass(ctx, class)
	}
}

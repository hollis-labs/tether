package consumers

import (
	"context"
	"fmt"

	"github.com/hollis-labs/tether/internal/app/routingcap"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// ChannelReader can be the local channel service or a daemon client. Keeping
// this port here lets standalone MCP consumers use daemon authorization.
type ChannelReader interface {
	List(context.Context, string) ([]channels.Channel, error)
	History(context.Context, string, string, int64, int) (channels.Page, error)
	Latest(context.Context, string, string, int) (channels.Page, error)
}

type RoutingReader interface {
	RoutingCapabilities(context.Context, string) (routingcap.RoutingCapabilitiesResponse, error)
}

// ConsumerService is the shared channel/capability consumer facade. Transports
// own argument decoding and framing; validation and pagination live here.
type ConsumerService struct {
	ChannelReader
	RoutingReader
}

type ChannelListPage struct {
	Channels   []channels.Channel `json:"channels"`
	NextOffset int                `json:"next_offset,omitempty"`
}

func (s ConsumerService) ListAll(ctx context.Context, as string) ([]channels.Channel, error) {
	if s.ChannelReader == nil {
		return nil, fmt.Errorf("channel consumer requires daemon routing")
	}
	return s.List(ctx, as)
}

func (s ConsumerService) ListPage(ctx context.Context, as string, offset, limit int) (ChannelListPage, error) {
	if offset < 0 || limit < 0 || limit > 1000 {
		return ChannelListPage{}, channels.ErrInvalid
	}
	if limit == 0 {
		limit = 100
	}
	if s.ChannelReader == nil {
		return ChannelListPage{}, fmt.Errorf("channel consumer requires daemon routing")
	}
	list, err := s.ListAll(ctx, as)
	if err != nil {
		return ChannelListPage{}, err
	}
	start := min(offset, len(list))
	end := start + min(limit, len(list)-start)
	page := ChannelListPage{Channels: append([]channels.Channel{}, list[start:end]...)}
	if end < len(list) {
		page.NextOffset = end
	}
	return page, nil
}

func (s ConsumerService) Read(ctx context.Context, name, as string, since int64, limit, last int) (channels.Page, error) {
	if err := channels.ValidateName(name); err != nil {
		return channels.Page{}, err
	}
	if since < 0 || limit < 0 || limit > 1000 || last < 0 || last > 1000 || (last > 0 && (since != 0 || limit != 0)) {
		return channels.Page{}, channels.ErrInvalid
	}
	if s.ChannelReader == nil {
		return channels.Page{}, fmt.Errorf("channel consumer requires daemon routing")
	}
	if last > 0 {
		return s.Latest(ctx, name, as, last)
	}
	return s.History(ctx, name, as, since, limit)
}

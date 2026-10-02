package channels

import (
	"context"
	"fmt"
	"time"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/identity"
)

type Channel struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type Message struct {
	Seq      int64      `json:"seq"`
	Purged   bool       `json:"purged,omitempty"`
	PurgedAt *time.Time `json:"purged_at,omitempty"`
	gomsg.Envelope
}

type Page struct {
	Channel
	Messages  []Message `json:"messages"`
	NextSince int64     `json:"next_since"`
}

// Backend persists publications in the normal message store and reads them by
// durable commit sequence. History never consumes or acknowledges a message.
type Backend interface {
	SetChannelAuthorization(Authorization)
	SendChannel(context.Context, gomsg.Envelope) (gomsg.Envelope, error)
	ListChannelNames(context.Context) ([]string, error)
	ReadChannel(context.Context, string, int64, int) ([]Message, error)
	ReadLatestChannel(context.Context, string, int) ([]Message, error)
	ChannelHighWater(context.Context, string) (int64, error)
}

// Authorization is the caller-identity hook shared by every transport. The
// default policy is observe-only: identity middleware controls authentication,
// and channels impose no membership rule. A future scope policy can install a
// checker without changing history, publishing or subscription semantics.
type Authorization func(ctx context.Context, operation, name string, caller identity.Principal, from gomsg.Address) error

type Service struct {
	backend Backend
	check   Authorization
}

func New(backend Backend, check Authorization) *Service {
	if check != nil {
		backend.SetChannelAuthorization(check)
	}
	return &Service{backend: backend, check: check}
}

func (s *Service) authorize(ctx context.Context, operation, name, asserted string) error {
	p, verified := identity.FromContext(ctx)
	if !verified {
		if _, err := gomsg.ParseURN(asserted); err != nil {
			return fmt.Errorf("%w: a valid caller identity (as) is required", ErrInvalid)
		}
		p.ID = asserted
	}
	if s.check != nil {
		return s.check(ctx, operation, name, p, gomsg.Address{})
	}
	return nil
}

func describe(name string) Channel {
	a, _ := ChannelAddress(name) // Names from the backend were validated on publish.
	return Channel{Name: name, Address: a.URN()}
}

func (s *Service) List(ctx context.Context, as string) ([]Channel, error) {
	if err := s.authorize(ctx, "list", "", as); err != nil {
		return nil, err
	}
	names, err := s.backend.ListChannelNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(names))
	for _, name := range names {
		out = append(out, describe(name))
	}
	return out, nil
}

func (s *Service) Publish(ctx context.Context, env gomsg.Envelope) (gomsg.Envelope, error) {
	publication, err := NormalizePublication(&env)
	if err != nil || !publication {
		return gomsg.Envelope{}, ErrInvalid
	}
	// Publication authorization lives at the backend's atomic insertion seam,
	// including dispatcher, HTTP and daemon-internal publishers.
	return s.backend.SendChannel(ctx, env)
}

func (s *Service) History(ctx context.Context, name, as string, since int64, limit int) (Page, error) {
	if err := ValidateName(name); err != nil {
		return Page{}, err
	}
	if since < 0 || limit < 0 || limit > 1000 {
		return Page{}, ErrInvalid
	}
	if err := s.authorize(ctx, "history", name, as); err != nil {
		return Page{}, err
	}
	if limit == 0 {
		limit = 100
	}
	return s.read(ctx, name, since, limit)
}

func (s *Service) read(ctx context.Context, name string, since int64, limit int) (Page, error) {
	rows, err := s.backend.ReadChannel(ctx, name, since, limit)
	if err != nil {
		return Page{}, err
	}
	if rows == nil {
		rows = []Message{}
	}
	next := since
	if len(rows) > 0 {
		next = rows[len(rows)-1].Seq
	}
	return Page{Channel: describe(name), Messages: rows, NextSince: next}, nil
}

// Latest returns the most recent count publications, oldest first.
func (s *Service) Latest(ctx context.Context, name, as string, count int) (Page, error) {
	if err := ValidateName(name); err != nil {
		return Page{}, err
	}
	if count < 1 || count > 1000 {
		return Page{}, ErrInvalid
	}
	if err := s.authorize(ctx, "history", name, as); err != nil {
		return Page{}, err
	}
	rows, err := s.backend.ReadLatestChannel(ctx, name, count)
	if err != nil {
		return Page{}, err
	}
	if rows == nil {
		rows = []Message{}
	}
	page := Page{Channel: describe(name), Messages: rows}
	if len(rows) > 0 {
		page.NextSince = rows[len(rows)-1].Seq
	}
	return page, nil
}

type Event struct {
	Message       Message
	Err           error
	InitialCursor *int64
}

// Subscribe replays strictly after since, then tails the same durable cursor.
// Nil since captures the current high-water mark (live-only); &0 replays all.
// Reads in bounded batches and backpressure avoid the mailbox fan-out's
// dropped-event behavior. Polling also observes commits from other processes.
// Cancel ctx when abandoning a stream; no subscriptions are stored as members.
func (s *Service) Subscribe(ctx context.Context, name, as string, since *int64) (<-chan Event, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, "subscribe", name, as); err != nil {
		return nil, err
	}
	high, err := s.backend.ChannelHighWater(ctx, name)
	if err != nil {
		return nil, err
	}
	cursor := high
	if since != nil {
		if *since < 0 {
			return nil, fmt.Errorf("%w: since is outside channel history", ErrInvalid)
		}
		cursor = min(*since, high)
	}
	out := make(chan Event)
	go func() {
		defer close(out)
		if since == nil {
			select {
			case out <- Event{InitialCursor: &high}:
			case <-ctx.Done():
				return
			}
		}
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			page, err := s.read(ctx, name, cursor, 100)
			if err != nil {
				select {
				case out <- Event{Err: err}:
				case <-ctx.Done():
				}
				return
			}
			for _, msg := range page.Messages {
				select {
				case out <- Event{Message: msg}:
					cursor = msg.Seq
				case <-ctx.Done():
					return
				}
			}
			if len(page.Messages) == 100 {
				continue
			}
			select {
			case <-tick.C:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

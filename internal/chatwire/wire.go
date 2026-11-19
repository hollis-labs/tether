package chatwire

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/agent/sdk/chathost"

	"github.com/hollis-labs/tether/internal/conversation"
	tstore "github.com/hollis-labs/tether/internal/store"
)

// Prefix is where internal/api mounts the handler.
const Prefix = "/chat/v1"

// Config configures Open.
type Config struct {
	// StateDir is the daemon's state directory. The chat database is StateDir/chat/chat.db and the chat host's
	// state StateDir/chat/host.
	StateDir string
	// Host is the chat host (OpenHost). Optional: Open opens StateDir/chat/host when nil. The daemon passes the one it
	// also gives terminal attachments, so the two share a writer lease.
	Host *chathost.Host
	// Store is the Tether store: the conversations the wire serves, and the relaunch path's moves.
	Store *tstore.Store
	// Sessions reaches the sessions that run them (*app.Service).
	Sessions Sessions
	// Authorize defaults to the package's Authorize.
	Authorize chat.Authorize
	// Logger defaults to slog.Default.
	Logger *slog.Logger
	// Service is passed to chat.NewService after the fields Open sets (Agents, Runner, Log, Control, Logger), for
	// tests that need a clock or shorter timeouts. Optional.
	Service func(*chat.Config)
}

// Wire is the chat wire the Tether daemon serves for its own seats (CW-20261118-0011): libs/agent/chat's handler over
// a Runner for Tether sessions and a Control over chathost, with the event log in the chat database.
type Wire struct {
	db       *DB
	host     *chathost.Host
	ctl      *Control
	runner   *Runner
	svc      *chat.Service
	handler  http.Handler
	store    *tstore.Store
	logger   *slog.Logger
	ctx      context.Context
	stop     context.CancelFunc
	resolver conversation.Resolver
}

// Open opens the chat database and the chat host under cfg.StateDir and builds the Service and handler. It installs
// the relaunch hook on cfg.Store (Store.OnConversationMoved); Close removes it.
func Open(ctx context.Context, cfg Config) (*Wire, error) {
	if cfg.StateDir == "" || cfg.Store == nil || cfg.Sessions == nil {
		return nil, errors.New("chatwire: Config needs StateDir, Store and Sessions")
	}
	if cfg.Authorize == nil {
		cfg.Authorize = Authorize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := &Wire{store: cfg.Store, logger: cfg.Logger, resolver: conversation.Resolver{Store: cfg.Store}}
	var err error
	if w.db, err = OpenDB(ctx, filepath.Join(cfg.StateDir, "chat", "chat.db")); err != nil {
		return nil, err
	}
	fail := func(err error) (*Wire, error) {
		_ = w.db.Close()
		return nil, err
	}
	// The Control is daemon-internal: the handler has authorized the caller before any Control call.
	if w.host = cfg.Host; w.host == nil {
		if w.host, err = OpenHost(cfg.StateDir); err != nil {
			return fail(err)
		}
	}
	w.ctl, err = NewControl(ControlConfig{
		DB: w.db.Store, Host: w.host,
		Seat: func(ctx context.Context, agent string) (string, error) {
			t, err := w.resolver.BySeat(ctx, agent)
			return t.ConversationID, err
		},
		Target: w.resolver.ByConversation,
	})
	if err != nil {
		return fail(err)
	}
	w.runner, err = NewRunner(RunnerConfig{Mapping: w.ctl, Target: w.resolver.ByConversation, Sessions: cfg.Sessions, Logger: cfg.Logger})
	if err != nil {
		return fail(err)
	}
	sc := chat.Config{Agents: w.agent, Runner: w.runner, Log: w.db.Log, Control: w.ctl, Logger: cfg.Logger}
	if cfg.Service != nil {
		cfg.Service(&sc)
	}
	if w.svc, err = chat.NewService(sc); err != nil {
		w.runner.Close()
		return fail(err)
	}
	w.runner.SetObserver(w.svc)
	if w.handler, err = chat.NewHandler(w.svc, cfg.Authorize); err != nil {
		_ = w.svc.Close(ctx)
		w.runner.Close()
		return fail(err)
	}
	w.ctx, w.stop = context.WithCancel(context.Background())
	cfg.Store.OnConversationMoved(w.conversationMoved)
	return w, nil
}

// Handler is the wire's routes, without the prefix.
func (w *Wire) Handler() http.Handler { return w.handler }

// Service is the chat Service (Go Submit, Observe).
func (w *Wire) Service() *chat.Service { return w.svc }

// Close removes the relaunch hook, ends every run with run.abort, and closes the backends and the database.
func (w *Wire) Close(ctx context.Context) error {
	w.store.OnConversationMoved(nil)
	w.stop()
	err := w.svc.Close(ctx)
	w.runner.Close()
	return errors.Join(err, w.db.Close())
}

// agent is the Service's agent resolver: an agent is a seat with a Tether conversation.
func (w *Wire) agent(ctx context.Context, name string) (chat.Agent, error) {
	t, err := w.resolver.BySeat(ctx, name)
	if errors.Is(err, conversation.ErrNotFound) {
		return chat.Agent{}, fmt.Errorf("no seat %q with a conversation: %w", name, chat.ErrNotFound)
	}
	if err != nil {
		return chat.Agent{}, err
	}
	return chat.Agent{Owner: t.Owner, Scope: t.Seat, Kind: t.Kind}, nil
}

// conversationMoved is the store's post-commit hook for a Tether conversation that moved to another session: the
// Control brings the chat record and the chat host binding to the new generation, then the runner moves to the new
// session and signals Replaced, and the Service publishes chat.generation. It runs off the committing goroutine.
func (w *Wire) conversationMoved(tetherConversationID string) {
	go func() {
		ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
		defer cancel()
		chatID, err := w.ctl.Sync(ctx, tetherConversationID)
		if errors.Is(err, chat.ErrNotFound) {
			return // no chat client has opened this conversation
		}
		if err != nil {
			w.logger.Error("chatwire: generation sync after a relaunch failed", "conversation", tetherConversationID, "error", err)
		}
		if chatID != "" {
			w.runner.Moved(ctx, chatID)
		}
	}()
}

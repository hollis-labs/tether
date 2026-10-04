package teamhost

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

type Host struct {
	db     *sql.DB
	store  *teamstore.Store
	ports  Ports
	tiers  map[mesh.DefinitionRef]TrustTier
	actors map[mesh.URN]TrustTier
	now    func() time.Time
}

// New attaches an already migrated database. db and storage must borrow the
// same handle. No port is called by construction.
func New(db *sql.DB, storage *teamstore.Store, ports Ports, options Options) (*Host, error) {
	if db == nil || storage == nil || ports.Sessions == nil || ports.Enroller == nil || ports.Messenger == nil || ports.Channels == nil {
		return nil, errors.New("team host: database, store and all four ports required")
	}
	tiers := make(map[mesh.DefinitionRef]TrustTier, len(options.TrustTiers))
	for pin, tier := range options.TrustTiers {
		if pin.ID == "" || pin.Revision == "" || (tier != TrustDenied && tier != TrustApproval && tier != TrustTrusted) {
			return nil, errors.New("team host: invalid trust tier")
		}
		tiers[pin] = tier
	}
	actors := make(map[mesh.URN]TrustTier, len(options.ActorTrust))
	for actor, tier := range options.ActorTrust {
		if actor.Validate() != nil || (tier != TrustDenied && tier != TrustApproval && tier != TrustTrusted) {
			return nil, errors.New("team host: invalid actor trust tier")
		}
		actors[actor] = tier
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	storage.SetLaunchWriteHook(writeLaunchIntents)
	return &Host{db: db, store: storage, ports: ports, tiers: tiers, actors: actors, now: now}, nil
}
func (h *Host) Now() time.Time { return h.now() }
func (h *Host) NewID() string  { return uuid.NewString() }
func id(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("team-%x", sum[:16])
}
func encode(v any) ([]byte, error) { return json.Marshal(v) }
func decode(b []byte, v any) error { return json.Unmarshal(b, v) }
func same[T any](payload []byte, value T) (bool, error) {
	var old, normalized T
	if err := decode(payload, &old); err != nil {
		return false, err
	}
	b, err := encode(value)
	if err != nil {
		return false, err
	}
	if err = decode(b, &normalized); err != nil {
		return false, err
	}
	return reflect.DeepEqual(old, normalized), nil
}
func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return teams.ErrNotFound
	}
	return err
}
func (h *Host) tx(ctx context.Context, fn func(*sql.Conn) error) error {
	return h.store.WithTransaction(ctx, fn)
}

var (
	_ teams.MemberProvisioner = (*Host)(nil)
	_ teams.WorkflowLauncher  = (*Host)(nil)
	_ teams.MessageSender     = (*Host)(nil)
	_ teams.DeliveryStore     = (*Host)(nil)
	_ teams.RoutingInstaller  = (*Host)(nil)
	_ teams.TrustResolver     = (*Host)(nil)
	_ teams.ApprovalEmitter   = (*Host)(nil)
	_ teams.TriggerEvaluator  = (*Host)(nil)
	_ teams.Clock             = (*Host)(nil)
	_ teams.IDs               = (*Host)(nil)
)

// writeLaunchIntents receives the record already normalized by PutLaunch. The
// lookup and launch record commit together without another encoding or decode.
func writeLaunchIntents(ctx context.Context, conn *sql.Conn, record teams.LaunchRecord) error {
	for _, intent := range record.Intents {
		if intent.Key == "" {
			continue
		}
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO team_host_launch_intents(intent_key,launch_key) VALUES(?,?)`, intent.Key, record.Key); err != nil {
			return err
		}
	}
	return nil
}

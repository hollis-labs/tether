package fabricbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

// Authority carries immutable lease identity; head versions stay inside the writer.
// The caller must equal Holder; owners cannot inject reports as another holder.
type Authority struct {
	AgentURN, SessionURN mesh.URN
	InstanceID           string
	Fence                uint64
}

func (a Authority) valid() bool {
	return urnOK(a.AgentURN) && urnOK(a.SessionURN) && textOK(a.InstanceID) && a.Fence > 0
}
func (s *Service) writerAuth(ctx context.Context, caller mesh.URN, a Authority, action Action, proof string) (fabricstore.Record[mesh.Agent], error) {
	if !a.valid() {
		return fabricstore.Record[mesh.Agent]{}, fabricstore.ErrInvalid
	}
	agent, err := s.agent(ctx, caller, a.AgentURN)
	if err != nil {
		return agent, err
	}
	err = s.allow(ctx, Authorization{Caller: caller, Owner: agent.Value.Owner, AgentURN: a.AgentURN, SessionURN: a.SessionURN, InstanceID: a.InstanceID, Action: action, Fence: a.Fence, ProofRef: proof})
	return agent, err
}
func (s *Service) held(tx *fabricstore.Tx, caller mesh.URN, a Authority, at time.Time) (fabricstore.Record[fabricstore.BindingHead], error) {
	head, err := tx.BindingHead(a.AgentURN)
	if errors.Is(err, fabricstore.ErrNotFound) {
		return head, ErrStale
	}
	if err != nil {
		return head, err
	}
	return s.checkHeld(head, caller, a, at)
}

// preHeld prevents a stale observation from reaching policy. The writer still
// checks the complete authority again after validation, under serialization.
func (s *Service) preHeld(ctx context.Context, caller mesh.URN, a Authority, at time.Time) error {
	head, err := s.repo.BindingHead(ctx, a.AgentURN)
	if errors.Is(err, fabricstore.ErrNotFound) {
		return ErrStale
	}
	if err != nil {
		return err
	}
	_, err = s.checkHeld(head, caller, a, at)
	return err
}
func (s *Service) checkHeld(head fabricstore.Record[fabricstore.BindingHead], caller mesh.URN, a Authority, at time.Time) (fabricstore.Record[fabricstore.BindingHead], error) {
	lease := head.Value.Lease
	checked, err := s.clock()
	if err != nil {
		return head, err
	}
	if lease == nil || lease.Holder != caller || lease.AgentURN != a.AgentURN || lease.SessionURN != a.SessionURN || lease.InstanceID != a.InstanceID || lease.FencingToken != a.Fence || head.Value.HighWater != a.Fence || !lease.ExpiresAt.After(at) || !lease.ExpiresAt.After(checked) {
		return head, ErrStale
	}
	return head, nil
}
func (s *Service) RenewLease(ctx context.Context, caller mesh.URN, a Authority, ttl time.Duration) (mesh.BindingLease, error) {
	var result mesh.BindingLease
	at, err := s.clock()
	if err != nil {
		return result, err
	}
	if !s.ttl(ttl) {
		return result, fabricstore.ErrInvalid
	}
	agent, err := s.writerAuth(ctx, caller, a, Renew, "")
	if err != nil {
		return result, err
	}
	err = s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		if e := checkAgent(tx, agent, true); e != nil {
			return e
		}
		head, e := s.held(tx, caller, a, at)
		if e != nil {
			return e
		}
		lease := *head.Value.Lease
		expires := at.Add(ttl)
		if !expires.After(lease.ExpiresAt) {
			return fabricstore.ErrInvalid
		}
		lease.ExpiresAt = expires
		head.Value.Lease = &lease
		if e = tx.PutBindingHead(head.Value, head.Version); e != nil {
			return e
		}
		id, e := digest([]any{a.Fence, a.AgentURN, lease.ExpiresAt, "renewed"})
		if e != nil {
			return e
		}
		if e = s.bindingEvent(tx, id, lease, "renewed", at); e != nil {
			return e
		}
		result = lease
		return nil
	})
	if err != nil {
		return mesh.BindingLease{}, err
	}
	return result, nil
}

// ReleaseLease requires host-authorized quiescence/death evidence. It does not
// infer process death or fabricate a terminal instance. HighWater never resets.
func (s *Service) ReleaseLease(ctx context.Context, caller mesh.URN, a Authority, proofRef string) error {
	at, err := s.clock()
	if err != nil {
		return err
	}
	if !textOK(proofRef) {
		return fabricstore.ErrInvalid
	}
	agent, err := s.writerAuth(ctx, caller, a, Release, proofRef)
	if err != nil {
		return err
	}
	return s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		if e := checkAgent(tx, agent, false); e != nil {
			return e
		}
		head, e := s.held(tx, caller, a, at)
		if e != nil {
			return e
		}
		lease := *head.Value.Lease
		head.Value.Lease = nil
		if e = tx.PutBindingHead(head.Value, head.Version); e != nil {
			return e
		}
		id, e := digest([]any{a, "released"})
		if e != nil {
			return e
		}
		return s.bindingEvent(tx, id, lease, "released", at)
	})
}

// Observation is the rejecting sink contract handed to a host. Both lifecycle
// and referenced reports carry exact agent/session/instance/holder/fence context.
// Payload content remains with its provider; only a bounded reference is stored.
type Observation struct {
	Authority                       Authority
	Status                          mesh.InstanceStatus
	Detail                          mesh.InstanceDetail
	SessionState                    mesh.SessionState
	SessionVersion, InstanceVersion int64
	ReportRef                       string
}
type HostPort interface {
	Observe(context.Context, mesh.URN, Observation) (ObservationResult, error)
	ReportReference(context.Context, mesh.URN, Authority, string) error
}

var _ HostPort = (*Service)(nil)

func terminal(status mesh.InstanceStatus) bool {
	return status == mesh.InstanceStopped || status == mesh.InstanceRejected
}
func transition(from, to mesh.InstanceStatus) bool {
	switch from {
	case mesh.InstanceStopped, mesh.InstanceRejected:
		return false
	case mesh.InstanceStarting:
		return to == mesh.InstanceStarting || to == mesh.InstanceRunning || to == mesh.InstanceWaiting || to == mesh.InstanceStopping || terminal(to)
	case mesh.InstanceRunning, mesh.InstanceWaiting:
		return to == mesh.InstanceRunning || to == mesh.InstanceWaiting || to == mesh.InstanceStopping || to == mesh.InstanceStopped
	case mesh.InstanceStopping:
		return to == mesh.InstanceStopping || to == mesh.InstanceStopped
	}
	return false
}

type ObservationResult struct{ SessionVersion, InstanceVersion int64 }

func (s *Service) Observe(ctx context.Context, caller mesh.URN, o Observation) (ObservationResult, error) {
	var result ObservationResult
	at, err := s.clock()
	if err != nil {
		return result, err
	}
	if o.InstanceVersion <= 0 || o.SessionVersion <= 0 || !textOK(o.ReportRef) {
		return result, fabricstore.ErrInvalid
	}
	if _, err = mesh.ProjectState(o.Status, o.Detail, o.SessionState); err != nil {
		return result, fabricstore.ErrInvalid
	}
	if terminal(o.Status) && (o.SessionState != mesh.SessionEnded && o.SessionState != mesh.SessionOrphaned) {
		return result, fabricstore.ErrInvalid
	}
	if !terminal(o.Status) && o.SessionState == mesh.SessionEnded {
		return result, fabricstore.ErrInvalid
	}
	agent, err := s.writerAuth(ctx, caller, o.Authority, Lifecycle, "")
	if err != nil {
		return result, err
	}
	if err = s.preHeld(ctx, caller, o.Authority, at); err != nil {
		return result, err
	}
	if err = s.validateObservation(ctx, caller, o); err != nil {
		return result, err
	}
	err = s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		if e := checkAgent(tx, agent, !terminal(o.Status)); e != nil {
			return e
		}
		head, e := s.held(tx, caller, o.Authority, at)
		if e != nil {
			return e
		}
		instance, e := tx.Instance(o.Authority.InstanceID)
		if e != nil {
			return e
		}
		session, e := tx.Session(o.Authority.SessionURN)
		if e != nil {
			return e
		}
		if instance.Version != o.InstanceVersion || session.Version != o.SessionVersion {
			return fabricstore.ErrConflict
		}
		if instance.Value.AgentURN != o.Authority.AgentURN || instance.Value.SessionURN != o.Authority.SessionURN || instance.Value.BindingFence != o.Authority.Fence || session.Value.AgentURN != o.Authority.AgentURN {
			return ErrStale
		}
		if !transition(instance.Value.Status, o.Status) {
			return fabricstore.ErrInvalid
		}
		instance.Value.Status = o.Status
		instance.Value.Detail = o.Detail
		instance.Value.UpdatedAt = at
		session.Value.State = o.SessionState
		session.Value.UpdatedAt = at
		if e = tx.PutSession(session.Value, session.Version); e != nil {
			return e
		}
		if e = tx.PutInstance(instance.Value, instance.Version); e != nil {
			return e
		}
		id, e := digest([]any{o.Authority, o.InstanceVersion, "observed"})
		if e != nil {
			return e
		}
		if e = appendEvent(tx, id, o.Authority.AgentURN, "instance.observed", o, at); e != nil {
			return e
		}
		result = ObservationResult{SessionVersion: session.Version + 1, InstanceVersion: instance.Version + 1}
		if terminal(o.Status) {
			lease := *head.Value.Lease
			head.Value.Lease = nil
			if e = tx.PutBindingHead(head.Value, head.Version); e != nil {
				return e
			}
			return s.bindingEvent(tx, id+"/released", lease, "released", at)
		}
		return nil
	})
	if err != nil {
		return ObservationResult{}, err
	}
	return result, nil
}

func (s *Service) ReportReference(ctx context.Context, caller mesh.URN, a Authority, ref string) error {
	at, err := s.clock()
	if err != nil {
		return err
	}
	if !textOK(ref) {
		return fabricstore.ErrInvalid
	}
	agent, err := s.writerAuth(ctx, caller, a, Report, "")
	if err != nil {
		return err
	}
	if err = s.preHeld(ctx, caller, a, at); err != nil {
		return err
	}
	observation := Observation{Authority: a, ReportRef: ref}
	if err = s.validateObservation(ctx, caller, observation); err != nil {
		return err
	}
	return s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		if e := checkAgent(tx, agent, true); e != nil {
			return e
		}
		if _, e := s.held(tx, caller, a, at); e != nil {
			return e
		}
		id, e := digest([]any{a, ref, "report"})
		if e != nil {
			return e
		}
		raw, e := json.Marshal(observation)
		if e != nil {
			return e
		}
		prior, e := tx.Event(id)
		if e == nil {
			if prior.AggregateURN != a.AgentURN || prior.Type != "instance.report" || string(prior.Payload) != string(raw) {
				return fabricstore.ErrConflict
			}
			return nil
		}
		if !errors.Is(e, fabricstore.ErrNotFound) {
			return e
		}
		return appendEvent(tx, id, a.AgentURN, "instance.report", observation, at)
	})
}
func appendEvent(tx *fabricstore.Tx, id string, urn mesh.URN, kind string, payload any, at time.Time) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal binding event: %w", err)
	}
	return tx.AppendEvent(fabricstore.Event{ID: id, AggregateURN: urn, Type: kind, Payload: raw, CreatedAt: at})
}

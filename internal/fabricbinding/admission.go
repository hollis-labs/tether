package fabricbinding

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

// AdmissionRequest names immutable reservation inputs. Holder is always the
// authenticated caller, never a request field. A new key reserves a new instance;
// an existing durable session preserves its pin/context/store references.
type AdmissionRequest struct {
	AgentURN, SessionURN                                                        mesh.URN
	InstanceID, Key, NodeRef, RuntimeRef, LaunchRecordRef, ContextRef, StoreRef string
	TTL                                                                         time.Duration
}
type Reservation struct {
	OperationRef string
	Session      mesh.Session
	Instance     mesh.AgentInstance
	// Lease is the ORIGINAL acquisition receipt, not a present authority claim.
	Lease mesh.BindingLease
}

func (r AdmissionRequest) valid() bool {
	return urnOK(r.AgentURN) && urnOK(r.SessionURN) && textOK(r.InstanceID) && textOK(r.Key) && textOK(r.NodeRef) && textOK(r.RuntimeRef) && textOK(r.LaunchRecordRef) && textOK(r.ContextRef) && textOK(r.StoreRef)
}
func admissionAuth(caller mesh.URN, agent mesh.Agent, r AdmissionRequest, pin mesh.DefinitionRef) Authorization {
	return Authorization{Caller: caller, Owner: agent.Owner, AgentURN: r.AgentURN, SessionURN: r.SessionURN, InstanceID: r.InstanceID, Definition: pin, Action: Acquire, NodeRef: r.NodeRef, RuntimeRef: r.RuntimeRef, LaunchRecordRef: r.LaunchRecordRef, ContextRef: r.ContextRef, StoreRef: r.StoreRef}
}
func reservation(tx *fabricstore.Tx, a fabricstore.Admission, requestDigest string) (Reservation, error) {
	var out Reservation
	if a.RequestDigest != requestDigest {
		return out, fabricstore.ErrConflict
	}
	session, err := tx.BindingEvent(a.OperationRef + "/acquired")
	if err != nil {
		return out, err
	}
	instance, err := tx.Instance(a.InstanceID)
	if err != nil {
		return out, err
	}
	record, err := tx.Session(instance.Value.SessionURN)
	if err != nil {
		return out, err
	}
	if session.Value.Lease.InstanceID != instance.Value.ID || session.Value.Lease.Holder != a.Caller || session.Value.Lease.SessionURN != record.Value.URN || session.Value.Lease.AgentURN != instance.Value.AgentURN || session.Value.Lease.FencingToken != instance.Value.BindingFence {
		return out, fabricstore.ErrInvalid
	}
	return Reservation{a.OperationRef, record.Value, instance.Value, session.Value.Lease}, nil
}

// Admit verifies pins outside the writer, then rechecks all identities and
// versions inside BEGIN IMMEDIATE. Retry never renews, reacquires or dispatches.
func (s *Service) Admit(ctx context.Context, caller mesh.URN, r AdmissionRequest) (Reservation, error) {
	var result Reservation
	at, err := s.clock()
	if err != nil {
		return result, err
	}
	if !r.valid() || !s.ttl(r.TTL) {
		return result, fabricstore.ErrInvalid
	}
	agent, err := s.agent(ctx, caller, r.AgentURN)
	if err != nil {
		return result, err
	}
	// Authorization precedes session lookup, preventing session existence probes.
	if err = s.allow(ctx, admissionAuth(caller, agent.Value, r, agent.Value.Definition)); err != nil {
		return result, err
	}
	session, err := s.repo.Session(ctx, r.SessionURN)
	newSession := errors.Is(err, fabricstore.ErrNotFound)
	if err != nil && !newSession {
		return result, err
	}
	pin := agent.Value.Definition
	if !newSession {
		if session.Value.AgentURN != r.AgentURN || session.Value.ContextRef != r.ContextRef || session.Value.StoreRef != r.StoreRef {
			return result, ErrDenied
		}
		pin = session.Value.Definition
		if err = s.allow(ctx, admissionAuth(caller, agent.Value, r, pin)); err != nil {
			return result, err
		}
	}
	requestDigest, err := digest(r)
	if err != nil {
		return result, err
	}
	operation, err := digest([]string{string(caller), r.Key})
	if err != nil {
		return result, err
	}
	// A committed identity reservation can be read on retry even if source bytes
	// became unavailable. It carries no renewed launch or writer authority.
	_, err = s.repo.Admission(ctx, caller, r.Key)
	if err != nil && !errors.Is(err, fabricstore.ErrNotFound) {
		return result, err
	}
	if err == nil {
		err = s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
			current, e := tx.Admission(caller, r.Key)
			if e != nil {
				return e
			}
			result, e = reservation(tx, current.Value, requestDigest)
			return e
		})
		if err != nil {
			return Reservation{}, err
		}
		return result, nil
	}
	if agent.Value.Lifecycle != mesh.EnrollmentActive {
		return result, ErrDenied
	}
	if err = s.verify(ctx, pin); err != nil {
		return result, err
	}
	expires := at.Add(r.TTL)
	err = s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		current, e := tx.Admission(caller, r.Key)
		if e == nil {
			result, e = reservation(tx, current.Value, requestDigest)
			return e
		}
		if !errors.Is(e, fabricstore.ErrNotFound) {
			return e
		}
		if e = checkAgent(tx, agent); e != nil {
			return e
		}
		held, e := tx.BindingHead(r.AgentURN)
		if e != nil && !errors.Is(e, fabricstore.ErrNotFound) {
			return e
		}
		if errors.Is(e, fabricstore.ErrNotFound) {
			held = fabricstore.Record[fabricstore.BindingHead]{Value: fabricstore.BindingHead{AgentURN: r.AgentURN}}
		}
		checked, e := s.clock()
		if e != nil {
			return e
		}
		if !expires.After(checked) {
			return ErrStale
		}
		if held.Value.Lease != nil && held.Value.Lease.ExpiresAt.After(checked) {
			return ErrBound
		}
		if held.Value.HighWater >= math.MaxInt64 {
			return fabricstore.ErrInvalid
		}
		actual, e := tx.Session(r.SessionURN)
		if newSession {
			if e == nil {
				return fabricstore.ErrConflict
			}
			if !errors.Is(e, fabricstore.ErrNotFound) {
				return e
			}
			session = fabricstore.Record[mesh.Session]{Value: mesh.Session{URN: r.SessionURN, AgentURN: r.AgentURN, Definition: pin, ContextRef: r.ContextRef, StoreRef: r.StoreRef, CreatedAt: at}}
		} else {
			if e != nil {
				return e
			}
			if actual != session {
				return fabricstore.ErrConflict
			}
		}
		session.Value.State = mesh.SessionStarting
		session.Value.UpdatedAt = at
		if e = tx.PutSession(session.Value, session.Version); e != nil {
			return e
		}
		fence := held.Value.HighWater + 1
		instance := mesh.AgentInstance{ID: r.InstanceID, AgentURN: r.AgentURN, SessionURN: r.SessionURN, Definition: pin, NodeRef: r.NodeRef, RuntimeRef: r.RuntimeRef, LaunchRecordRef: r.LaunchRecordRef, BindingFence: fence, Status: mesh.InstanceStarting, CreatedAt: at, UpdatedAt: at}
		if e = tx.PutInstance(instance, 0); e != nil {
			return e
		}
		lease := mesh.BindingLease{AgentURN: r.AgentURN, SessionURN: r.SessionURN, InstanceID: r.InstanceID, Holder: caller, ExpiresAt: expires, FencingToken: fence}
		old := held.Value.Lease
		if e = tx.PutBindingHead(fabricstore.BindingHead{AgentURN: r.AgentURN, HighWater: fence, Lease: &lease}, held.Version); e != nil {
			return e
		}
		if old != nil {
			if e = s.bindingEvent(tx, operation+"/expired", *old, "expired", at); e != nil {
				return e
			}
		}
		if e = s.bindingEvent(tx, operation+"/acquired", lease, "acquired", at); e != nil {
			return e
		}
		if e = tx.PutAdmission(fabricstore.Admission{Caller: caller, Key: r.Key, RequestDigest: requestDigest, OperationRef: operation, InstanceID: r.InstanceID, State: "committed"}, 0); e != nil {
			return e
		}
		result = Reservation{operation, session.Value, instance, lease}
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return result, nil
}
func (s *Service) bindingEvent(tx *fabricstore.Tx, id string, lease mesh.BindingLease, kind string, at time.Time) error {
	if err := tx.AddBindingEvent(fabricstore.BindingEvent{ID: id, Lease: lease, Kind: kind, At: at}); err != nil {
		return err
	}
	return appendEvent(tx, id, lease.AgentURN, "binding."+kind, lease, at)
}

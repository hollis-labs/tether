package environmentstream

import (
	"encoding/json"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// Reduce assigns state from durable events. Replaying the same event is safe;
// disconnected sessions never become successful terminal work implicitly.
func Reduce(session *Session, event Event) {
	if event.SessionID != session.ID {
		return
	}
	if event.Time.After(session.LastActivity) {
		session.LastActivity = event.Time
	}
	if event.Kind == events.KindSessionStateChanged {
		var change struct {
			To string `json:"to"`
		}
		if json.Unmarshal(event.Payload, &change) == nil && change.To != "" {
			setState(session, change.To)
		}
	}
	if event.Kind == store.EnvironmentRequestKind {
		var req store.EnvironmentRequest
		if json.Unmarshal(event.Payload, &req) == nil {
			applyRequest(session, req)
		}
	}
}

func applyRequest(s *Session, req store.EnvironmentRequest) {
	if s.SessionState == mesh.SessionEnded {
		return
	}
	if s.requests == nil {
		s.requests = make(map[string]store.EnvironmentRequest)
	}
	key := req.TurnID + "\x00" + req.RequestID
	if previous, exists := s.requests[key]; exists {
		if req.SourceSequence < previous.SourceSequence {
			return
		}
		req.Kind = previous.Kind
	} else if !req.Open {
		return
	}
	s.requests[key] = req
	refreshRequests(s)
}

func refreshRequests(s *Session) {
	if s.SessionState == mesh.SessionEnded {
		return
	}
	s.PendingQuestion = false
	s.PendingApproval = false
	for _, request := range s.requests {
		switch request.Kind {
		case "question":
			s.QuestionKnown = true
			s.PendingQuestion = s.PendingQuestion || request.Open
		case "approval":
			s.ApprovalKnown = true
			s.PendingApproval = s.PendingApproval || request.Open
		}
	}
	s.PendingKnown = s.QuestionKnown && s.ApprovalKnown
	if s.PendingQuestion || s.PendingApproval {
		s.InstanceStatus = mesh.InstanceWaiting
		if s.PendingApproval {
			s.InstanceDetail.Waiting = mesh.WaitingApproval
		} else {
			s.InstanceDetail.Waiting = mesh.WaitingInput
		}
	} else {
		s.InstanceStatus = mesh.InstanceRunning
		s.InstanceDetail = mesh.InstanceDetail{}
	}
}

func setState(s *Session, state string) {
	s.InstanceDetail = mesh.InstanceDetail{}
	switch state {
	case "created", "launching", "starting":
		s.SessionState = mesh.SessionStarting
		s.InstanceStatus = mesh.InstanceStarting
	case "running":
		s.SessionState = mesh.SessionRunning
		s.InstanceStatus = mesh.InstanceRunning
	case "detached":
		s.SessionState = mesh.SessionDetached
		s.InstanceStatus = mesh.InstanceRunning
	case "orphaned":
		s.SessionState = mesh.SessionOrphaned
		s.InstanceStatus = mesh.InstanceRunning
	case "paused":
		s.SessionState = mesh.SessionPaused
		s.InstanceStatus = mesh.InstanceRunning
	case "stopping":
		s.SessionState = mesh.SessionRunning
		s.InstanceStatus = mesh.InstanceStopping
	case "completed":
		s.SessionState = mesh.SessionEnded
		s.InstanceStatus = mesh.InstanceStopped
		s.InstanceDetail.Stopped = mesh.StopCompleted
		s.PendingKnown = true
	case "failed":
		s.SessionState = mesh.SessionEnded
		s.InstanceStatus = mesh.InstanceStopped
		s.InstanceDetail.Stopped = mesh.StopFailed
		s.PendingKnown = true
	case "killed":
		s.SessionState = mesh.SessionEnded
		s.InstanceStatus = mesh.InstanceStopped
		s.InstanceDetail.Stopped = mesh.StopCanceled
		s.PendingKnown = true
	default: // Unknown connectivity cannot imply an outcome.
		s.SessionState = mesh.SessionOrphaned
		s.InstanceStatus = mesh.InstanceRunning
	}
	if s.SessionState == mesh.SessionEnded {
		s.PendingQuestion = false
		s.PendingApproval = false
		s.PendingKnown = true
		s.QuestionKnown = true
		s.ApprovalKnown = true
		s.requests = nil
	}
	if len(s.requests) > 0 {
		refreshRequests(s)
	}
}

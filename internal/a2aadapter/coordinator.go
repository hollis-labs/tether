package a2aadapter

import (
	"context"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// taskOutcome is what a consumer's explicit transition call (transition.go)
// delivers to a blocked Execute call (executor.go) for one delegated task.
type taskOutcome struct {
	state   a2a.TaskState
	message *a2a.Message
}

// pendingTask is one delegated task an Execute call is blocked on,
// together with the binding that created it. Tracking bindingID here (not
// just the task) is what lets resolve reject a transition call that
// names the wrong binding for a given task id — closing a real
// cross-binding task-hijack gap an independent review of this diff found:
// task IDs are process-wide unique UUIDs, and a single taskCoordinator is
// shared across every binding (see adapter.go), so without this check any
// caller who could reach ANY binding's transition endpoint could resolve
// a DIFFERENT binding's task, bypassing that other binding's own
// authorization entirely.
type pendingTask struct {
	bindingID string
	ch        chan taskOutcome
}

// taskCoordinator is the in-memory hand-off point between an Execute call
// waiting on a delegated task and the consumer-owned transition endpoint
// that resolves it. Deliberately ephemeral (process-lifetime only, never
// persisted) — per this package's doc comment, Tether relays delegated
// work into its own canonical messaging and waits for an explicit
// authorized signal back; it does not durably own task outcome state
// itself, that responsibility stays with the consumer.
type taskCoordinator struct {
	mu      sync.Mutex
	pending map[a2a.TaskID]*pendingTask
}

func newTaskCoordinator() *taskCoordinator {
	return &taskCoordinator{pending: make(map[a2a.TaskID]*pendingTask)}
}

// register creates the wait channel for taskID, owned by bindingID. Must
// be called before await/resolve race — executor.go calls it
// synchronously before yielding the Working status update, so a
// transition call arriving immediately after can never be lost.
func (c *taskCoordinator) register(taskID a2a.TaskID, bindingID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[taskID] = &pendingTask{bindingID: bindingID, ch: make(chan taskOutcome, 1)}
}

// await blocks until either a transition resolves taskID, ctx is
// canceled, or the timeout elapses. The second return is false in the
// latter two cases; the caller (executor.go) treats a false as "no
// consumer transition arrived in time," not as an error.
func (c *taskCoordinator) await(ctx context.Context, taskID a2a.TaskID, timeoutC <-chan time.Time) (taskOutcome, bool) {
	c.mu.Lock()
	pt, ok := c.pending[taskID]
	c.mu.Unlock()
	if !ok {
		return taskOutcome{}, false
	}
	defer c.forget(taskID)

	select {
	case outcome := <-pt.ch:
		return outcome, true
	case <-ctx.Done():
		return taskOutcome{}, false
	case <-timeoutC:
		return taskOutcome{}, false
	}
}

// resolve delivers outcome to a pending await for taskID, but ONLY when
// bindingID matches the binding that registered it. Returns
// (resolved=false, wrongBinding=true) rather than (false, false) for a
// binding mismatch specifically, so transition.go's HTTP layer can
// report a 404 ("no such task on THIS binding") instead of a 409
// ("already resolved") -- the two are different failures worth telling
// apart. resolved=false/wrongBinding=false covers "unknown task, already
// resolved, or its Execute call already gave up on timeout" -- an honest
// non-success in all three cases, never a false success.
func (c *taskCoordinator) resolve(taskID a2a.TaskID, bindingID string, outcome taskOutcome) (resolved, wrongBinding bool) {
	c.mu.Lock()
	pt, ok := c.pending[taskID]
	c.mu.Unlock()
	if !ok {
		return false, false
	}
	if pt.bindingID != bindingID {
		return false, true
	}
	select {
	case pt.ch <- outcome:
		return true, false
	default:
		// Already resolved (buffered channel already holds a value) or
		// the awaiter already gave up and is about to forget() — either
		// way a second resolve for the same task is not this call's to
		// report as a fresh success.
		return false, false
	}
}

func (c *taskCoordinator) forget(taskID a2a.TaskID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, taskID)
}

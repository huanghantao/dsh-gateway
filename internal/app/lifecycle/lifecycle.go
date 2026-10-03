// Package lifecycle owns the gateway's own operational state: whether it is
// serving, and whether it is on its way out.
//
// It exists because a process with long-lived work has three states, not two.
// "Running" and "stopped" are not enough when a turn can take ten minutes and a
// deploy wants to be over in seconds: there is a window in which the gateway is
// still answering, still finishing what it started, and must not accept anything
// new. Without a name for that window, every caller invents its own — a shutdown
// flag read in one handler and forgotten in another, a check that races the
// thing it is checking — and the failure is not a crash but a prompt admitted by
// a process that is about to close the agent underneath it.
//
// So the state is one value with one owner, and every path that could start work
// asks it. The rule is deliberately asymmetric: draining refuses *new* work and
// lets existing work finish. Stopping work that is already running is what the
// drain is for, not something it does.
package lifecycle

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Reason explains why a gateway is draining, for the log and the frame.
type Reason string

const (
	// ReasonShutdown is a signal: a redeploy, a stop, a logout.
	ReasonShutdown Reason = "the gateway is shutting down"
	// ReasonDeploy is a redeploy specifically. It is separate from shutdown so
	// that a client can say "updating" rather than "stopping".
	ReasonDeploy Reason = "the gateway is being redeployed"
)

// State is a gateway's operational state.
type State string

const (
	// StateServing means new work is accepted.
	StateServing State = "serving"
	// StateDraining means work already admitted may finish, and nothing new is
	// accepted.
	StateDraining State = "draining"
)

// Lifecycle is the gateway's own state machine. It is safe for concurrent use.
type Lifecycle struct {
	state atomic.Value // State
	bus   *events.Bus
	busMu sync.Mutex
	log   *logx.Logger
	// once guard so the draining frame is published exactly once however many
	// callers race to be the one that noticed.
	once sync.Once
}

// New builds a Lifecycle in the serving state. bus may be nil, in which case
// transitions are logged and not published — which is what a test wants.
func New(bus *events.Bus, logger *logx.Logger) *Lifecycle {
	if logger == nil {
		logger = logx.Discard()
	}
	l := &Lifecycle{bus: bus, log: logger}
	l.state.Store(StateServing)
	return l
}

// State reports the current state.
func (l *Lifecycle) State() State {
	state, _ := l.state.Load().(State)
	if state == "" {
		return StateServing
	}
	return state
}

// Serving reports whether new work may be admitted. It is the check every
// work-admitting path makes.
func (l *Lifecycle) Serving() bool { return l.State() == StateServing }

// Draining reports whether the gateway is on its way out.
func (l *Lifecycle) Draining() bool { return !l.Serving() }

// Drain enters the draining state and tells connected clients, once.
//
// Telling them is the point, and it is why this is not a bare boolean. A client
// that is only disconnected discovers the redeploy by failing; a client that is
// told can move first — reconnect to the successor before the old listener goes
// away, and stop offering to start work that would be refused. The frame is
// published exactly once so that a second caller racing the first cannot produce
// two notices for one deploy.
func (l *Lifecycle) Drain(reason Reason) {
	l.once.Do(func() {
		l.state.Store(StateDraining)
		text := string(reason)
		l.log.Info("draining", "reason", text)
		l.busMu.Lock()
		defer l.busMu.Unlock()
		if l.bus == nil {
			return
		}
		l.bus.Publish(events.TypeDraining, "", events.Draining{Reason: text})
	})
}

// ErrDraining is the refusal a work-admitting path returns while draining.
//
// A distinct code rather than a generic conflict, because a client has a useful
// thing to do with it — wait and retry — and a generic "conflict" would have it
// re-prompting the user for a decision that has not changed.
func ErrDraining() error {
	return errx.New(errx.KindUnavailable, "gateway_draining",
		"the gateway is being redeployed and is not accepting new work; retry in a moment")
}

// Admit reports whether new work may start, and the error to return if not.
func (l *Lifecycle) Admit() error {
	if l.Draining() {
		return ErrDraining()
	}
	return nil
}

// WaitFor polls until busy reports that nothing is in flight, the context is
// cancelled, or the window expires. It returns whether the gateway became idle.
//
// Polling rather than an event is deliberate: the alternative is a condition
// variable shared with the turn scheduler, which would put a second consumer on
// its lock and a second definition of "busy" in the process. This runs twice a
// second for the length of a deploy, which is not a cost worth that coupling.
func WaitFor(ctx Done, busy func() bool, window time.Duration, poll time.Duration) bool {
	if busy == nil {
		return true
	}
	if !busy() {
		return true
	}
	if window <= 0 {
		return false
	}
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return !busy()
		case <-deadline.C:
			return !busy()
		case <-ticker.C:
			if !busy() {
				return true
			}
		}
	}
}

// Done is the part of context.Context WaitFor needs. Narrowing it keeps the
// helper usable from a test with a channel and from a caller with a context.
type Done interface{ Done() <-chan struct{} }

package v1

import (
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/turns"
)

// snapshot assembles the present state a client needs to render the truth
// without a round trip.
//
// It is built from the same application services the live frames are built
// from — the bus's own watermark, the harness's state, the turn scheduler's
// queues, the approval broker's pending table — so a snapshot can never say
// something the stream would not have said. scope is the set of session ids the
// connection asked about, or nil for all of them.
//
// A snapshot is deliberately not a transcript. Conversation rows come from the
// session log, which the client already fetches and pages through; what the
// client cannot reconstruct after a gap is *what is happening now* — a turn that
// started while it was away, a queue that moved, a decision waiting for it —
// and that is exactly what this carries.
func (s *Server) snapshot(scope []string, at uint64) snapshotFrame {
	return snapshotFrame{
		Snapshot: events.Snapshot{
			Generation: s.deps.Bus.Generation(),
			Seq:        at,
			Time:       time.Now().UTC(),
			Harness:    events.HarnessState{State: string(s.deps.Harness.State())},
			Turns:      s.turnSnapshot(scope),
		},
		Approvals: s.approvalSnapshot(scope),
	}
}

// turnSnapshot reports one entry per session that has a turn, in or waiting.
func (s *Server) turnSnapshot(scope []string) []events.SessionTurn {
	if s.deps.Turns == nil {
		return nil
	}
	out := make([]events.SessionTurn, 0, len(scope))
	// An unscoped connection wants every session with a turn. There is no
	// enumeration of them on the scheduler — it only answers about sessions it
	// knows — so the sessions come from the same place a client's list does.
	for _, sessionID := range s.snapshotSessions(scope) {
		queue := s.deps.Turns.Queue(sessionID)
		if !queue.Busy() && len(queue.Queued) == 0 {
			continue
		}
		entry := events.SessionTurn{SessionID: sessionID}
		if queue.Running != nil {
			running := turnState(*queue.Running, len(queue.Queued))
			entry.Running = &running
		}
		for i, queued := range queue.Queued {
			entry.Queued = append(entry.Queued, turnState(queued, len(queue.Queued)-i-1))
		}
		out = append(out, entry)
	}
	return out
}

// snapshotSessions is the set of session ids a snapshot should describe: the
// scope when the connection gave one, and the sessions a client would list
// otherwise.
//
// The unscoped case is answered from the leased sessions rather than from the
// session store, deliberately. A turn can only be running for a session this
// gateway holds — the lease is what makes it the writer — so the leased set is
// both complete for the question being asked and free.
func (s *Server) snapshotSessions(scope []string) []string {
	if len(scope) > 0 {
		return scope
	}
	if s.deps.Leases == nil {
		return nil
	}
	held := s.deps.Leases.List()
	out := make([]string, 0, len(held))
	for _, lease := range held {
		out = append(out, lease.SessionID)
	}
	return out
}

// turnState renders a ticket the way the live `turn.state` frame renders it, so
// a client applies a snapshot through exactly the same code path as an event.
func turnState(t turns.Ticket, behind int) events.TurnState {
	state := events.TurnState{
		TurnID:     t.ID,
		State:      t.State,
		Position:   t.Position,
		QueueDepth: behind,
		QueuedAt:   timePtr(t.QueuedAt),
		StartedAt:  t.StartedAt,
	}
	// A settled turn carried no position on the stream either, and a client that
	// rendered one would draw a queue entry for a turn that is over.
	if t.State != turns.StateQueued {
		state.Position = 0
	}
	return state
}

// approvalSnapshot lists the decisions waiting for a person, in the shape
// `GET /approvals` returns them.
func (s *Server) approvalSnapshot(scope []string) []approvals.View {
	if s.deps.Approvals == nil {
		return nil
	}
	pending := s.deps.Approvals.List()
	if len(scope) == 0 {
		return pending
	}
	inScope := make(map[string]struct{}, len(scope))
	for _, id := range scope {
		inScope[id] = struct{}{}
	}
	out := make([]approvals.View, 0, len(pending))
	for _, view := range pending {
		if _, ok := inScope[view.SessionID]; ok {
			out = append(out, view)
		}
	}
	return out
}

// timePtr takes the address of a time only when it is set, so a zero queuedAt
// does not reach the wire as year one.
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Package approvals brokers human authorisation of tool calls.
//
// DeepSeek Harness asks permission before running a tool whose policy requires
// it. Over ACP that arrives as a `session/request_permission` request, which the
// harness adapter must answer. This package is the connection between that
// blocking question and a phone screen: it parks the request, announces it, and
// waits for a human.
//
// The security property that matters most here is fail-closed. Every path that
// does not end in an explicit human "allow" ends in a refusal:
//
//   - No decision within the timeout: refused.
//   - The broker is shut down while a request is pending: refused.
//   - A decision naming an option the harness never offered: rejected as invalid,
//     and the request stays open rather than being resolved by a malformed input.
//
// There is deliberately no "allow always" and no auto-approve. A tool the
// operator has not seen is a tool the operator has not authorised.
package approvals

import (
	"context"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Option is one choice offered to the operator.
type Option struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// View is the wire representation of a pending approval.
type View struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"sessionId"`
	ToolCallID  string    `json:"toolCallId"`
	Tool        string    `json:"tool"`
	Input       string    `json:"input"`
	RequestedAt time.Time `json:"requestedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Options     []Option  `json:"options"`
}

// Broker implements harness.PermissionHandler.
type Broker struct {
	timeout time.Duration
	now     func() time.Time
	logger  *logx.Logger
	bus     *events.Bus

	mu      sync.Mutex
	pending map[string]*waiter
	closed  bool
}

type waiter struct {
	view   View
	answer chan harness.PermissionDecision
}

// New builds a Broker.
func New(timeout time.Duration, bus *events.Bus, logger *logx.Logger, now func() time.Time) *Broker {
	if now == nil {
		now = time.Now
	}
	return &Broker{
		timeout: timeout,
		now:     now,
		logger:  logger,
		bus:     bus,
		pending: map[string]*waiter{},
	}
}

// RequestPermission implements harness.PermissionHandler.
//
// It blocks until a human decides or the context expires. On expiry it returns an
// error, which the harness adapter turns into a refusal — the tool does not run.
func (b *Broker) RequestPermission(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	view := View{
		ID:          req.ID,
		SessionID:   req.SessionID,
		ToolCallID:  req.ToolCallID,
		Tool:        req.Tool,
		Input:       req.Input,
		RequestedAt: req.RequestedAt,
		ExpiresAt:   req.ExpiresAt,
	}
	for _, o := range req.Options {
		view.Options = append(view.Options, Option{ID: o.ID, Name: o.Name})
	}
	if view.ExpiresAt.IsZero() {
		view.ExpiresAt = b.now().Add(b.timeout)
	}

	w := &waiter{view: view, answer: make(chan harness.PermissionDecision, 1)}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return harness.PermissionDecision{}, errx.New(errx.KindUnavailable, "approvals_closed",
			"the gateway is shutting down")
	}
	// A duplicate id would leave the earlier waiter unreachable, so replace it
	// and let the older one time out. DSH tool-call ids are unique in practice;
	// this is defence against a harness bug, not an expected path.
	b.pending[view.ID] = w
	b.mu.Unlock()

	b.bus.Publish(events.TypeApprovalRequested, view.SessionID, view)
	b.logger.Info("approval requested",
		"approval", view.ID, "session", view.SessionID, "tool", view.Tool)

	defer func() {
		b.mu.Lock()
		// Only clear our own entry: a replacement may already be installed.
		if cur, ok := b.pending[view.ID]; ok && cur == w {
			delete(b.pending, view.ID)
		}
		b.mu.Unlock()
	}()

	select {
	case d := <-w.answer:
		if d.DecidedBy == "" {
			d.DecidedBy = "operator"
		}
		b.bus.Publish(events.TypeApprovalResolved, view.SessionID, map[string]any{
			"id": view.ID, "optionId": d.OptionID, "decidedBy": d.DecidedBy,
		})
		return d, nil

	case <-ctx.Done():
		// Fail closed. Announce the refusal so every client dismisses the sheet
		// instead of leaving a dead prompt on screen.
		b.bus.Publish(events.TypeApprovalResolved, view.SessionID, map[string]any{
			"id": view.ID, "optionId": harness.OptionRejectOnce, "decidedBy": "timeout",
		})
		b.logger.Warn("approval expired without a decision; refusing",
			"approval", view.ID, "session", view.SessionID, "tool", view.Tool)
		return harness.PermissionDecision{}, errx.New(errx.KindTimeout, "approval_timeout",
			"nobody answered the approval request in time, so the tool was refused")
	}
}

// Decide resolves a pending approval.
//
// optionID must be one of the options the harness offered. Accepting an arbitrary
// string would let a client invent a choice the agent never agreed to, which is
// exactly the kind of confusion an approval flow must not have.
func (b *Broker) Decide(ctx context.Context, id, optionID, decidedBy string) error {
	b.mu.Lock()
	w, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return errx.New(errx.KindConflict, "approval_closed",
			"that approval is no longer pending; it was decided or it expired")
	}
	if !offered(w.view.Options, optionID) {
		b.mu.Unlock()
		return errx.New(errx.KindInvalid, "unknown_option",
			"that choice was not offered for this request")
	}
	delete(b.pending, id)
	b.mu.Unlock()

	// Buffered with capacity one and written exactly once, so this never blocks.
	w.answer <- harness.PermissionDecision{OptionID: optionID, DecidedBy: decidedBy}
	b.logger.Info("approval decided",
		"approval", id, "session", w.view.SessionID, "tool", w.view.Tool,
		"option", optionID, "by", decidedBy)
	return nil
}

// List returns pending approvals, oldest first so the UI shows the most urgent
// decision at the top.
func (b *Broker) List() []View {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]View, 0, len(b.pending))
	for _, w := range b.pending {
		out = append(out, w.view)
	}
	// Insertion sort by request time: the pending set is tiny, and this avoids
	// pulling in a sort dependency for a handful of elements.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].RequestedAt.Before(out[j-1].RequestedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Get returns one pending approval.
func (b *Broker) Get(id string) (View, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, ok := b.pending[id]
	if !ok {
		return View{}, false
	}
	return w.view, true
}

// Close refuses every pending approval. It is called during shutdown so that no
// tool is left waiting on a gateway that is going away.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	waiters := make([]*waiter, 0, len(b.pending))
	for _, w := range b.pending {
		waiters = append(waiters, w)
	}
	b.pending = map[string]*waiter{}
	b.mu.Unlock()

	for _, w := range waiters {
		// Wake each waiter so RequestPermission returns immediately rather than
		// sitting until its context expires.
		select {
		case w.answer <- harness.PermissionDecision{OptionID: harness.OptionRejectOnce, DecidedBy: "shutdown"}:
		default:
		}
	}
}

// offered reports whether optionID appears in options.
func offered(options []Option, optionID string) bool {
	for _, o := range options {
		if o.ID == optionID {
			return true
		}
	}
	return false
}

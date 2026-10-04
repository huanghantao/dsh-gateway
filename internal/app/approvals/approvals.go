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
//   - A standing grant that does not match the session, the tool and — for an
//     exact grant — the arguments: not applied.
//
// There is no auto-approve and no way to turn prompts off. What a decision can
// do is name a *scope*: the operator answers once for "this tool in this
// session", and the broker remembers it for as long as the configured grant TTL.
// The property that is preserved is the one that matters — no tool runs that a
// human did not authorise — and what changes is that the authorisation can be a
// decision instead of a reflex. See docs/adr/0005.
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
	// Grant is true for an option this gateway synthesised, as opposed to one
	// the harness offered. A client that wants to present "allow always" as a
	// different kind of decision from "allow this once" needs to be able to tell
	// them apart without matching on ids it would have to keep in sync.
	Grant bool `json:"grant,omitempty"`
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
	timeout  time.Duration
	grantTTL time.Duration
	now      func() time.Time
	logger   *logx.Logger
	bus      *events.Bus

	mu      sync.Mutex
	pending map[string]*waiter
	grants  map[string]Grant
	closed  bool
}

type waiter struct {
	view   View
	answer chan harness.PermissionDecision
}

// Options configures a Broker.
type Options struct {
	// Timeout bounds how long one approval waits for a human.
	Timeout time.Duration
	// GrantTTL bounds a scoped decision. Zero disables scoped grants entirely:
	// the synthesised options are not offered, and every invocation needs its
	// own answer.
	GrantTTL time.Duration
	Bus      *events.Bus
	Logger   *logx.Logger
	Now      func() time.Time
}

// New builds a Broker.
func New(opts Options) *Broker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	return &Broker{
		timeout:  opts.Timeout,
		grantTTL: opts.GrantTTL,
		now:      opts.Now,
		logger:   opts.Logger,
		bus:      opts.Bus,
		pending:  map[string]*waiter{},
	}
}

// RequestPermission implements harness.PermissionHandler.
//
// It blocks until a human decides, a standing grant answers, or the approval's
// deadline passes. On expiry it returns an error, which the harness adapter turns
// into a refusal — the tool does not run.
//
// The deadline is the broker's own, not only the caller's: a request that arrives
// over the agent-host socket carries that connection's context, which outlives
// any approval, so waiting on the context alone would mean waiting forever.
func (b *Broker) RequestPermission(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
	view := b.buildView(req)

	// A standing grant answers without waking anybody. It is checked before the
	// request is announced so that a phone is never asked about something the
	// operator already decided.
	if b.grantTTL > 0 {
		if grant, ok := b.matchGrant(view.SessionID, view.Tool, req.Input); ok {
			b.bus.Publish(events.TypeApprovalGranted, view.SessionID, Granted{
				Grant: grant, Tool: view.Tool, Input: view.Input,
			})
			b.logger.Info("approval answered by a standing grant",
				"approval", view.ID, "session", view.SessionID,
				"tool", view.Tool, "grant", grant.ID)
			return harness.PermissionDecision{
				OptionID:  harness.OptionAllowOnce,
				DecidedBy: "grant",
			}, nil
		}
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

	// The wait is bounded twice on purpose, and neither bound substitutes for
	// the other.
	//
	// The caller's context is the in-process path: the ACP adapter hands down an
	// approval deadline with it and expects a refusal once it passes. The timer
	// below covers the other path, where a permission request is relayed from
	// the agent host: that context belongs to the *connection*, which is meant to
	// live for months, so it can never expire an approval. Without the timer a
	// prompt nobody answers is never resolved at all — the card stays in every
	// snapshot, the operator never gets told the tool was refused, and this
	// goroutine stays parked until the socket drops.
	//
	// It reads the deadline the view already carries rather than b.timeout,
	// because the requester may have named an earlier one and that promise is
	// the one on screen.
	wait := view.ExpiresAt.Sub(b.now())
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case d := <-w.answer:
		if d.DecidedBy == "" {
			d.DecidedBy = "operator"
		}
		b.bus.Publish(events.TypeApprovalResolved, view.SessionID, events.ApprovalDecision{
			ID: view.ID, OptionID: d.OptionID, DecidedBy: d.DecidedBy,
			Tool: view.Tool, SessionID: view.SessionID,
		})
		return d, nil

	case <-timer.C:
		return b.expire(view)

	case <-ctx.Done():
		return b.expire(view)
	}
}

// expire refuses an approval nobody answered in time, and says so out loud.
//
// Fail closed, and announce it: the refusal is what dismisses the sheet on every
// client instead of leaving a dead prompt on screen, it is what tells an operator
// who missed the notification that the tool did not run, and it is what the push
// notifier turns into "Approval expired".
func (b *Broker) expire(view View) (harness.PermissionDecision, error) {
	b.bus.Publish(events.TypeApprovalResolved, view.SessionID, events.ApprovalDecision{
		ID: view.ID, OptionID: harness.OptionRejectOnce, DecidedBy: "timeout",
		Tool: view.Tool, SessionID: view.SessionID,
	})
	b.logger.Warn("approval expired without a decision; refusing",
		"approval", view.ID, "session", view.SessionID, "tool", view.Tool)
	return harness.PermissionDecision{}, errx.New(errx.KindTimeout, "approval_timeout",
		"nobody answered the approval request in time, so the tool was refused")
}

// buildView renders a request for the wire and decides which choices to offer.
func (b *Broker) buildView(req harness.PermissionRequest) View {
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
	view.Options = append(view.Options, b.grantOptions(view.Options, view.Tool)...)
	return view
}

// grantOptions are the scoped choices this gateway adds to the harness's own.
//
// They are only offered when a grant could actually be recorded — grants
// enabled, and the harness offering an affirmative option for the broker to
// answer with. Adding "allow for this session" to a request whose only real
// answer is no would be offering the operator something the gateway cannot
// deliver.
func (b *Broker) grantOptions(existing []Option, tool string) []Option {
	if b.grantTTL <= 0 || !offered(existing, harness.OptionAllowOnce) {
		return nil
	}
	name := tool
	if name == "" {
		name = "this tool"
	}
	return []Option{
		{
			ID:    OptionAllowSessionTool,
			Name:  "Allow " + name + " in this session",
			Grant: true,
		},
		{
			ID:    OptionAllowExact,
			Name:  "Allow this exact call",
			Grant: true,
		},
	}
}

// Decide resolves a pending approval.
//
// optionID must be one of the options the request was shown with. Accepting an
// arbitrary string would let a client invent a choice the agent never agreed to,
// which is exactly the kind of confusion an approval flow must not have.
//
// A scoped choice records a grant and answers the harness with allow-once,
// because that is the only affirmative option DSH has. The scope lives here, on
// the side that talks to the human.
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

	answer := harness.PermissionDecision{OptionID: optionID, DecidedBy: decidedBy}
	if scope, ok := grantScope(optionID); ok {
		grant := b.recordGrant(w.view.SessionID, w.view.Tool, scope, w.view.Input, decidedBy)
		answer.OptionID = harness.OptionAllowOnce
		b.bus.Publish(events.TypeApprovalGranted, w.view.SessionID, Granted{
			Grant: grant, Tool: w.view.Tool, Input: w.view.Input,
		})
	}

	// Buffered with capacity one and written exactly once, so this never blocks.
	w.answer <- answer
	b.logger.Info("approval decided",
		"approval", id, "session", w.view.SessionID, "tool", w.view.Tool,
		"option", optionID, "by", decidedBy)
	return nil
}

// grantScope maps a scoped option id onto the scope it records.
func grantScope(optionID string) (string, bool) {
	switch optionID {
	case OptionAllowSessionTool:
		return ScopeTool, true
	case OptionAllowExact:
		return ScopeExact, true
	default:
		return "", false
	}
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

// offered is the one membership test: Decide refuses an option the request did
// not carry, and grantOptions uses it to require that an affirmative one exists.
func offered(options []Option, optionID string) bool {
	for _, o := range options {
		if o.ID == optionID {
			return true
		}
	}
	return false
}

package push

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// DefaultTurnThreshold is how long a turn must run before its end is worth a
// notification.
//
// The point is not to report every answer — a phone that buzzes for a two-second
// reply is a phone whose notifications get turned off — but to cover the case
// where someone prompts, puts the phone down, and needs to know when the work is
// done.
const DefaultTurnThreshold = 2 * time.Minute

// Notifier turns bus events into notifications.
//
// The policy is deliberately narrow, and it is written as one switch in handle
// so that "what earns an interruption" is a thing a reader can check rather than
// a property of five scattered methods. Everything it sends answers one
// question: **is the agent blocked on a human, or has something happened that
// will not announce itself again?**
//
//   - an approval request, which expires and is rejected if nobody answers;
//   - an approval that expired, because the operator who missed the first
//     notification would otherwise never learn the tool was refused;
//   - a turn that has finished, once it has run long enough that nobody is
//     watching it any more;
//   - a turn that failed, at any length: a failure leaves no result to come back
//     to, so it is the one outcome where staying silent costs the operator the
//     whole point of the turn;
//   - a harness that gave up, which nothing else will report.
//
// Everything else — a message arriving while the reader watches, a session
// resync, a model catalog change — is silent by construction. The default case
// is silence rather than an empty branch, because the safe direction for an
// event type added later is not to wake anyone.
type Notifier struct {
	bus     *events.Bus
	service *Service
	// webhooks are chat channels, which exist because Web Push on Android is
	// Google's push service and nothing else: a phone that cannot reach it can
	// never be notified, however correct this end is.
	webhooks  []Webhook
	logger    *logx.Logger
	threshold time.Duration
	now       func() time.Time
	// includeName decides whether a notification body may name the session. Off
	// by default; see NotifierOptions.IncludeSessionName.
	includeName bool
	// describe names a session for the notification body. Optional: without it
	// the message still says which session, by id.
	describe func(ctx context.Context, sessionID string) string

	mu      sync.Mutex
	running map[string]time.Time

	// ready is closed once the notifier is subscribed. A caller may wait on it
	// to know that an approval arriving this instant will not be missed, which
	// is also what a test needs in order not to race the goroutine it started.
	ready     chan struct{}
	readyOnce sync.Once
}

// NotifierOptions configures a Notifier.
type NotifierOptions struct {
	Bus *events.Bus
	// Service may be nil when only chat channels are configured.
	Service   *Service
	Webhooks  []Webhook
	Logger    *logx.Logger
	Threshold time.Duration
	Now       func() time.Time
	Describe  func(ctx context.Context, sessionID string) string
	// IncludeSessionName allows a notification body to carry the session's title
	// (or, failing that, the first line of the prompt that opened it).
	//
	// False by default and worth leaving false unless it is wanted: the body is
	// displayed on a lock screen, retained by the operating system's notification
	// store, and sent verbatim to any chat webhook that is configured. Everything
	// the notification needs in order to work — that an approval is waiting, that
	// a turn finished — is said without it.
	IncludeSessionName bool
}

// NewNotifier builds a Notifier.
func NewNotifier(opts NotifierOptions) (*Notifier, error) {
	if opts.Bus == nil {
		return nil, fmt.Errorf("push: a notifier needs a bus")
	}
	if opts.Service == nil && len(opts.Webhooks) == 0 {
		return nil, fmt.Errorf("push: a notifier needs at least one channel")
	}
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultTurnThreshold
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Notifier{
		bus:         opts.Bus,
		service:     opts.Service,
		webhooks:    opts.Webhooks,
		logger:      opts.Logger,
		threshold:   opts.Threshold,
		now:         opts.Now,
		describe:    opts.Describe,
		includeName: opts.IncludeSessionName,
		running:     map[string]time.Time{},
		ready:       make(chan struct{}),
	}, nil
}

// Run consumes events until the context is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	// Everything, from the beginning of what the ring holds: the notifier has no
	// bookmark of its own and must not miss an approval because it started a
	// moment late.
	sub, _ := n.bus.Subscribe(0, nil)
	defer sub.Close()
	n.readyOnce.Do(func() { close(n.ready) })

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-sub.Events():
			if !ok {
				return
			}
			n.handle(ctx, event)
		}
	}
}

// Ready is closed once the notifier is listening.
func (n *Notifier) Ready() <-chan struct{} { return n.ready }

// handle decides what one event means.
func (n *Notifier) handle(ctx context.Context, event events.Event) {
	switch event.Type {
	case events.TypeTurnState:
		n.handleTurn(ctx, event)
	case events.TypeApprovalRequested:
		n.handleApproval(ctx, event)
	case events.TypeApprovalResolved:
		n.handleApprovalResolved(ctx, event)
	case events.TypeHarnessState:
		n.handleHarness(ctx, event)
	default:
		// Nine frame types reach the bus and the rest are silent on purpose. See
		// the policy in the type comment above.
	}
}

func (n *Notifier) handleTurn(ctx context.Context, event events.Event) {
	// The payload is typed, so a producer and this consumer cannot disagree
	// about its shape without the build failing. That is not a hypothetical: the
	// approval branch below used to read a map its producer never published, and
	// the only symptom was a notification with no tool name in it.
	state, ok := event.Data.(events.TurnState)
	if !ok {
		n.mismatch(event, "events.TurnState")
		return
	}

	switch state.State {
	case "running", "queued":
		if state.State != "running" {
			return
		}
		n.mu.Lock()
		if _, known := n.running[event.SessionID]; !known {
			n.running[event.SessionID] = n.now()
		}
		n.mu.Unlock()

	case "completed", "cancelled", "failed":
		n.mu.Lock()
		started, known := n.running[event.SessionID]
		delete(n.running, event.SessionID)
		n.mu.Unlock()
		if !known {
			return
		}
		// A failure interrupts whatever its length. Everything else waits for the
		// threshold, because a short successful answer is something the operator
		// is still looking at — but a failure is the one outcome that leaves
		// nothing behind to come back to, and a prompt sent from a phone is
		// exactly the kind that is sent before walking away.
		if state.State != "failed" && n.now().Sub(started) < n.threshold {
			return
		}
		n.notify(ctx, Message{
			Title:     turnTitle(state.State),
			Body:      n.turnBody(ctx, event.SessionID, state),
			URL:       conversationURL(event.SessionID),
			Tag:       "turn-" + event.SessionID,
			SessionID: event.SessionID,
		}, turnUrgency(state.State))
	}
}

// turnTitle names the outcome.
//
// A failed turn used to arrive titled "The agent finished", which is true in the
// narrowest sense and useless on a lock screen: the one thing the operator needs
// to know is the one thing the title did not say.
func turnTitle(state string) string {
	switch state {
	case "failed":
		return "The turn failed"
	case "cancelled":
		return "The turn was stopped"
	default:
		return "The agent finished"
	}
}

// turnUrgency asks for attention in proportion to what went wrong.
func turnUrgency(state string) string {
	if state == "failed" {
		return "high"
	}
	return "normal"
}

// turnBody describes the settled turn without naming the session unless asked.
func (n *Notifier) turnBody(ctx context.Context, sessionID string, state events.TurnState) string {
	if detail := strings.TrimSpace(state.Detail); detail != "" {
		return truncate(detail)
	}
	if label := n.label(ctx, sessionID); label != "" {
		return label
	}
	if state.State == "failed" {
		return "Open the session for the error."
	}
	return "Open the session for the result."
}

func (n *Notifier) handleApproval(ctx context.Context, event events.Event) {
	view, ok := event.Data.(approvals.View)
	if !ok {
		n.mismatch(event, "approvals.View")
		return
	}
	body := "The agent is waiting for your decision."
	if view.Tool != "" {
		body = fmt.Sprintf("Approve %s?", view.Tool)
	}
	// The tool name is the harness's own vocabulary and says what is being asked;
	// the session title is the operator's and is only added on request.
	if label := n.label(ctx, event.SessionID); label != "" {
		body = fmt.Sprintf("%s · %s", body, label)
	}
	n.notify(ctx, Message{
		Title: "Approval needed",
		Body:  body,
		URL:   conversationURL(event.SessionID),
		// One approval replaces the last notification for the same one, and a
		// second approval in another session does not stack behind it.
		Tag:       "approval-" + view.ID,
		SessionID: event.SessionID,
	}, "high")
}

// handleApprovalResolved reports a decision nobody made.
//
// A human decision is not news: whoever tapped it was holding the phone. An
// expiry is, and it is the case the product used to lose entirely — the
// operator who missed the "Approval needed" notification because they were
// driving never learned that the tool was refused and the agent carried on
// without it. The notification it replaces is the one that asked.
func (n *Notifier) handleApprovalResolved(ctx context.Context, event events.Event) {
	decision, ok := event.Data.(events.ApprovalDecision)
	if !ok {
		n.mismatch(event, "events.ApprovalDecision")
		return
	}
	if decision.DecidedBy != "timeout" && decision.DecidedBy != "shutdown" {
		return
	}
	body := "Nobody answered, so the tool was refused."
	if decision.Tool != "" {
		body = fmt.Sprintf("%s was refused: nobody answered in time.", decision.Tool)
	}
	if label := n.label(ctx, event.SessionID); label != "" {
		body = fmt.Sprintf("%s · %s", body, label)
	}
	n.notify(ctx, Message{
		Title:     "Approval expired",
		Body:      body,
		URL:       conversationURL(event.SessionID),
		Tag:       "approval-" + decision.ID,
		SessionID: event.SessionID,
	}, "high")
}

// handleHarness reports the child process giving up.
//
// A state change is normally not worth waking anyone for — the supervisor
// restarts a child that died, and a buzz per restart would be noise from a
// self-healing condition. "Failed" is the state where it stopped healing: the
// supervisor has given up, every session is unusable, and nothing will say so
// until the operator opens the app and finds it dead.
func (n *Notifier) handleHarness(ctx context.Context, event events.Event) {
	state, ok := event.Data.(events.HarnessState)
	if !ok {
		n.mismatch(event, "events.HarnessState")
		return
	}
	if state.State != "failed" {
		return
	}
	body := "The agent is not running and could not be restarted."
	if detail := strings.TrimSpace(state.Detail); detail != "" {
		body = truncate(detail)
	}
	n.notify(ctx, Message{
		Title: "The agent stopped",
		Body:  body,
		URL:   "./#/sessions",
		// One standing condition, one notification: a supervisor that retries
		// must not leave a stack of identical complaints.
		Tag: "harness",
	}, "high")
}

// mismatch reports a payload this build cannot read.
//
// It is a warning rather than silence because silence is how the bug this
// replaced survived: the approval notifier asserted a map shape that no producer
// emitted, so every approval notification lost the tool name and shared one tag,
// and nothing anywhere said so.
func (n *Notifier) mismatch(event events.Event, want string) {
	if n.logger == nil {
		return
	}
	n.logger.Warn("push: ignoring an event whose payload this build cannot read",
		"type", string(event.Type), "got", fmt.Sprintf("%T", event.Data), "want", want)
}

// truncate keeps a failure detail to something a lock screen can show.
func truncate(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	const limit = 160
	if len(detail) <= limit {
		return detail
	}
	cut := limit
	for cut > 0 && !isRuneStart(detail[cut]) {
		cut--
	}
	return strings.TrimSpace(detail[:cut]) + "…"
}

// isRuneStart reports whether b begins a UTF-8 sequence, so a multi-byte
// character is never cut in half — a truncated Chinese error message must not
// become mojibake on a lock screen.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// notify sends to every channel. Failures are logged, never fatal: a phone that
// is off must not affect the agent, and one channel being down must not stop the
// other.
func (n *Notifier) notify(ctx context.Context, message Message, urgency string) {
	if n.service != nil {
		sent, errs := n.service.Broadcast(ctx, message, urgency)
		if n.logger != nil {
			if sent == 0 && len(errs) == 0 {
				n.logger.Debug("push: no browser subscribed", "title", message.Title)
			}
			for _, err := range errs {
				n.logger.Warn("push: could not notify a browser", "error", err.Error())
			}
		}
	}
	for i := range n.webhooks {
		hook := &n.webhooks[i]
		if err := hook.Send(ctx, message); err != nil && n.logger != nil {
			n.logger.Warn("push: could not notify a chat channel", "kind", hook.Kind, "error", err.Error())
		}
	}
}

// Channels names what this notifier will deliver to, for a status screen and for
// the startup line an operator reads when nothing arrives.
func (n *Notifier) Channels() []string {
	names := make([]string, 0, len(n.webhooks)+1)
	if n.service != nil {
		names = append(names, "webpush")
	}
	for _, hook := range n.webhooks {
		names = append(names, hook.Kind+"@"+hook.Host())
	}
	return names
}

// label returns the session's own name, or "" when naming it is not wanted.
//
// It is the single gate on the operator's words reaching a notification: every
// caller that would embed a title goes through here, so turning
// IncludeSessionName off cannot leave one path still leaking it.
func (n *Notifier) label(ctx context.Context, sessionID string) string {
	if !n.includeName || n.describe == nil {
		return ""
	}
	return strings.TrimSpace(n.describe(ctx, sessionID))
}

// conversationURL is where a tap lands: the app's own route for one session.
func conversationURL(sessionID string) string {
	return "./#/sessions/" + sessionID
}

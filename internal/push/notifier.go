package push

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

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
// Two events earn an interruption, and they are the two where the agent is
// blocked on a human:
//
//   - an approval request, which expires and is rejected if nobody answers;
//   - a turn that ran long enough that nobody is watching it any more.
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
	default:
		// Two events earn an interruption and the rest of the bus does not: a
		// message arrives while the reader is watching, a resync is housekeeping,
		// and a harness state change is not worth waking a phone for. Written as
		// a default rather than nine empty cases because the safe direction for a
		// new event type is silence — a notifier that buzzes for everything gets
		// muted, and then the two that matter are missed as well.
	}
}

func (n *Notifier) handleTurn(ctx context.Context, event events.Event) {
	data, ok := event.Data.(map[string]any)
	if !ok {
		return
	}
	state, _ := data["state"].(string)

	switch state {
	case "running":
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
		if elapsed := n.now().Sub(started); elapsed < n.threshold {
			return
		}
		n.notify(ctx, Message{
			Title:     "The agent finished",
			Body:      n.name(ctx, event.SessionID, state),
			URL:       conversationURL(event.SessionID),
			Tag:       "turn-" + event.SessionID,
			SessionID: event.SessionID,
		}, "normal")
	}
}

func (n *Notifier) handleApproval(ctx context.Context, event events.Event) {
	data, _ := event.Data.(map[string]any)
	tool, _ := data["tool"].(string)
	id, _ := data["id"].(string)
	body := "The agent is waiting for your decision."
	if tool != "" {
		body = fmt.Sprintf("Approve %s?", tool)
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
		Tag:       "approval-" + id,
		SessionID: event.SessionID,
	}, "high")
}

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

// name describes a session, falling back to something short and true.
//
// The fallback is what makes a generic body useful rather than merely terse: it
// still distinguishes a turn that failed from one that finished.
func (n *Notifier) name(ctx context.Context, sessionID, state string) string {
	if label := n.label(ctx, sessionID); label != "" {
		return label
	}
	if state == "failed" {
		return "The turn failed."
	}
	return "Open the session for the result."
}

// conversationURL is where a tap lands: the app's own route for one session.
func conversationURL(sessionID string) string {
	return "./#/sessions/" + sessionID
}

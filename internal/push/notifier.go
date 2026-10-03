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
//   - a delegated task that has finished, for the same reason — the operator who
//     handed work to a child agent and walked away is not watching the parent
//     turn either, and a report that arrives only when the parent settles can be
//     many minutes after the answer existed;
//   - a turn that failed, at any length: a failure leaves no result to come back
//     to, so it is the one outcome where staying silent costs the operator the
//     whole point of the turn;
//   - a harness that gave up, which nothing else will report.
//
// Everything else — a message arriving while the reader watches, a session
// resync, a model catalog change — is silent by construction. The default case
// is silence rather than an empty branch, because the safe direction for an
// event type added later is not to wake anyone.
//
// What each notification *says* is the other half of the design, and it lives in
// activity.go: an actor, an outcome and a summary, because "the agent finished"
// is not enough to act on when the session was running three agents.
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
	// includeTask decides whether a delegated task may be named by what it was
	// asked to do. See NotifierOptions.IncludeTaskNames.
	includeTask bool
	// describe names a session. Optional: without it the message still says
	// which actor settled and what it did, and says which session by id.
	describe func(ctx context.Context, sessionID string) string

	mu sync.Mutex
	// runs is the per-session ledger: what the current turn has accumulated,
	// and whether it is still going.
	runs map[string]*sessionRun
	// calls are the tool calls still in flight, held across the turn they belong
	// to so a delegation that outlives the registry's own correlation window can
	// still be named. See rememberCall.
	calls map[string]*openCall
	// order bounds calls by arrival, so a long-lived gateway cannot accumulate
	// them without limit.
	order []string

	// ready is closed once the notifier is subscribed. A caller may wait on it
	// to know that an approval arriving this instant will not be missed, which
	// is also what a test needs in order not to race the goroutine it started.
	ready     chan struct{}
	readyOnce sync.Once
}

// sessionRun is one session's current turn, as the notifier sees it.
type sessionRun struct {
	started time.Time
	// live is false once the turn has settled. It stays in the map so a tool
	// frame that arrives after the turn ended — the two producers do not
	// interleave perfectly — is not mistaken for a new turn's work.
	live   bool
	digest Digest
}

// openCall is a tool call that has started and not settled.
type openCall struct {
	sessionID string
	tool      string
	started   time.Time
	// task is the delegation's own name, empty for anything that is not one.
	task string
}

// maxTrackedCalls bounds the in-flight call registry. A session running more
// tools than this at once is not a case worth growing memory for; the oldest are
// dropped and their notifications simply name the tool instead of the task.
const maxTrackedCalls = 256

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
	// the notification needs in order to work — which actor settled and what it
	// did — is said without it.
	IncludeSessionName bool
	// IncludeTaskNames allows a notification to name a delegated task by its own
	// description.
	//
	// True by default, and a different decision from IncludeSessionName: a task
	// description is written by the model to summarise work it was handed, not
	// text the operator typed. Without it, three delegations finishing in one
	// session produce three identical "Subagent finished" notifications, which is
	// the confusion this whole vocabulary exists to remove.
	IncludeTaskNames bool
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
		includeTask: opts.IncludeTaskNames,
		runs:        map[string]*sessionRun{},
		calls:       map[string]*openCall{},
		ready:       make(chan struct{}),
	}, nil
}

// Run consumes events until the context is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	// A zero cursor means "from now". The notifier deliberately does not resume
	// from the ring: it has no bookmark of its own, and a notification about a
	// turn that ended before this process started is noise rather than news.
	sub := n.bus.Subscribe(events.Cursor{}, nil).Subscription
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
	case events.TypeSessionTool:
		n.handleTool(ctx, event)
	case events.TypeApprovalRequested:
		n.handleApproval(ctx, event)
	case events.TypeApprovalResolved:
		n.handleApprovalResolved(ctx, event)
	case events.TypeHarnessState:
		n.handleHarness(ctx, event)
	default:
		// The frame types this switch does not name are silent on purpose. See
		// the policy in the type comment above.
	}
}

// turnStart decides when a turn began, for the purpose of "was it long enough
// to interrupt someone about".
//
// The payload's own StartedAt is believed when it is present, and this is not a
// detail: the notifier is a late observer by nature — it is a subscriber that
// may attach at any point during a turn — so "the moment I first saw it running"
// is only the start when the turn began after this process was listening. For a
// turn already in flight it understates the wait, and worse, it makes the answer
// depend on when a notification happened to be scheduled. A turn with two
// minutes of work behind it would be suppressed as "too short to bother you
// with" if its first running frame was processed a moment before the reader put
// the phone down.
//
// The fallback is for the one producer that legitimately has no start time: the
// session-log watcher cannot know when a turn it did not start began, which is
// why the field is optional in the first place.
func turnStart(state events.TurnState, now func() time.Time) time.Time {
	if state.StartedAt != nil && !state.StartedAt.IsZero() {
		return *state.StartedAt
	}
	return now()
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
		run, known := n.runs[event.SessionID]
		switch {
		case !known || !run.live:
			// A new turn, or the first frame of a turn this process attached
			// mid-flight. Either way the ledger starts empty: work counted under
			// a previous turn must not be reported as this one's.
			n.runs[event.SessionID] = &sessionRun{started: turnStart(state, n.now), live: true}
		case run.started.IsZero():
			run.started = turnStart(state, n.now)
		}
		n.mu.Unlock()

	case "completed", "cancelled", "failed":
		n.mu.Lock()
		run, known := n.runs[event.SessionID]
		var started time.Time
		var digest Digest
		if known {
			started = run.started
			digest = run.digest
			run.live = false
		}
		n.mu.Unlock()
		if !known {
			return
		}
		// A failure interrupts whatever its length. Everything else waits for the
		// threshold, because a short successful answer is something the operator
		// is still looking at — but a failure is the one outcome that leaves
		// nothing behind to come back to, and a prompt sent from a phone is
		// exactly the kind that is sent before walking away.
		interrupt := state.State == "failed" || n.now().Sub(started) >= n.threshold
		n.notify(ctx, n.turnMessage(ctx, event.SessionID, state, digest), turnUrgency(state.State), interrupt)
	}
}

// turnMessage states one settled turn: who, what outcome, and what it amounted
// to.
//
// The summary is the ledger this notifier kept from tool frames. It is the same
// count the phone can make for a turn it watched, and for a turn it did not —
// the app was closed, the phone was in a pocket — this is the only place the
// number exists at all.
func (n *Notifier) turnMessage(ctx context.Context, sessionID string, state events.TurnState, digest Digest) Message {
	actor := Actor{Kind: ActorMain}
	summary := digest.Summary()
	title := activityTitle(state.State, actor, n.name(ctx, sessionID))
	body := activityBody(actor, summary, n.settledDetail(state))
	if body == "" {
		// A turn that counted nothing — a question answered in prose — still has
		// to say something, and the operator's own session name says more than
		// "open the session" whenever the deployment allows it.
		body = n.label(ctx, sessionID)
	}
	if body == "" {
		body = "Open the session for the result."
		if state.State == "failed" {
			body = "Open the session for the error."
		}
	}
	return Message{
		Title:     title,
		Body:      body,
		URL:       conversationURL(sessionID),
		Tag:       "turn-" + sessionID,
		SessionID: sessionID,
		Actor:     &actor,
		Summary:   summary,
		Outcome:   outcomeWord(state.State),
	}
}

// settledDetail is the explanation a settled turn carries, when it has one.
//
// A cancelled turn's detail is suppressed on purpose: the scheduler fills it
// with the error a cancel surfaced as, and printing that would tell the operator
// their agent broke when they were the one who stopped it.
func (n *Notifier) settledDetail(state events.TurnState) string {
	if state.State == "cancelled" {
		return ""
	}
	return strings.TrimSpace(state.Detail)
}

// handleTool folds a tool call into the turn's ledger, and reports a delegated
// task when it settles.
//
// Delegations are the reason this branch exists. A `subagent` call is a whole
// second agent running to completion inside the turn, and until it was read here
// its finish was invisible: a phone learned that the parent turn had ended, and
// nothing at all about the child that had answered ten minutes earlier.
func (n *Notifier) handleTool(ctx context.Context, event events.Event) {
	data, ok := event.Data.(events.ToolData)
	if !ok {
		n.mismatch(event, "events.ToolData")
		return
	}
	sessionID := event.SessionID
	tool := normaliseTool(data.Tool)

	if data.Phase == events.ToolStarted {
		n.rememberCall(data.CallID, &openCall{
			sessionID: sessionID,
			tool:      tool,
			started:   n.now(),
			task:      n.taskName(data.Tool, data.Input),
		})
		return
	}

	call, had := n.forgetCall(data.CallID)
	// What the call *is* comes from the opening frame, because the closing one
	// carries neither a title nor the arguments: DSH's completion update repeats
	// them for nobody. The name is therefore resolved here, once, rather than in
	// each consumer — the digest and the delegation branch below have to agree
	// about what ran, and the version of this that normalised the name twice
	// classified a settled `subagent` call as an unknown tool named "".
	name := data.Tool
	if name == "" && had {
		name = call.tool
	}
	fact := settledFact(name, data)

	n.mu.Lock()
	run, known := n.runs[sessionID]
	if known && run.live {
		run.digest = run.digest.add(fact)
	}
	n.mu.Unlock()

	if !fact.delegation {
		return
	}
	actor := Actor{Kind: ActorSub}
	if had {
		actor.Name = call.task
	}
	if actor.Name == "" {
		actor.Name = n.taskName(data.Tool, data.Input)
	}
	started := time.Time{}
	if had {
		started = call.started
	}
	state := "completed"
	if fact.failed {
		state = "failed"
	}
	interrupt := state == "failed" || (!started.IsZero() && n.now().Sub(started) >= n.threshold)
	detail := ""
	if state == "failed" {
		detail = strings.TrimSpace(data.Output)
	}
	message := Message{
		Title:     activityTitle(state, actor, n.name(ctx, sessionID)),
		Body:      activityBody(actor, "", detail),
		URL:       conversationURL(sessionID),
		Tag:       "task-" + data.CallID,
		SessionID: sessionID,
		Actor:     &actor,
		Outcome:   outcomeWord(state),
	}
	if message.Body == "" {
		message.Body = "Open the session for the report."
		if state == "failed" {
			message.Body = "Open the session for the error."
		}
	}
	n.notify(ctx, message, turnUrgency(state), interrupt)
}

// taskName is the delegation's own name, gated by the deployment's privacy
// choice. It returns "" both when the task is unnamed and when naming it is not
// wanted, because the notification treats those the same way: it falls back to
// the actor alone.
func (n *Notifier) taskName(tool, input string) string {
	if !n.includeTask || !delegationTool(normaliseTool(tool)) {
		return ""
	}
	return taskLabel(input)
}

// rememberCall records a call that has started.
func (n *Notifier) rememberCall(callID string, call *openCall) {
	if callID == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, exists := n.calls[callID]; !exists {
		n.order = append(n.order, callID)
	}
	n.calls[callID] = call
	for len(n.order) > maxTrackedCalls {
		oldest := n.order[0]
		n.order = n.order[1:]
		delete(n.calls, oldest)
	}
}

// forgetCall removes a settled call and returns what was known about it.
func (n *Notifier) forgetCall(callID string) (*openCall, bool) {
	if callID == "" {
		return nil, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	call, ok := n.calls[callID]
	if !ok {
		return nil, false
	}
	delete(n.calls, callID)
	for index, id := range n.order {
		if id == callID {
			n.order = append(n.order[:index], n.order[index+1:]...)
			break
		}
	}
	return call, true
}

// turnUrgency asks for attention in proportion to what went wrong.
func turnUrgency(state string) string {
	if state == "failed" {
		return "high"
	}
	return "normal"
}

func (n *Notifier) handleApproval(ctx context.Context, event events.Event) {
	view, ok := event.Data.(approvals.View)
	if !ok {
		n.mismatch(event, "approvals.View")
		return
	}
	actor := Actor{Kind: ActorMain}
	title := "Approval needed"
	if view.Tool != "" {
		title = fmt.Sprintf("Approval needed · %s", view.Tool)
	}
	body := "The agent is waiting for your decision."
	// The tool name is the harness's own vocabulary and says what is being asked;
	// the session title is the operator's and is only added where the deployment
	// allows it to appear.
	if label := n.label(ctx, event.SessionID); label != "" {
		body = fmt.Sprintf("%s · %s", body, label)
	}
	n.notify(ctx, Message{
		Title: title,
		Body:  body,
		URL:   conversationURL(event.SessionID),
		// One approval replaces the last notification for the same one, and a
		// second approval in another session does not stack behind it.
		Tag:       "approval-" + view.ID,
		SessionID: event.SessionID,
		Actor:     &actor,
		Outcome:   "waiting",
		Summary:   strings.TrimSpace(view.Tool),
	}, "high", true)
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
	actor := Actor{Kind: ActorSystem}
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
		Actor:     &actor,
		Outcome:   "expired",
		Summary:   strings.TrimSpace(decision.Tool),
	}, "high", true)
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
	actor := Actor{Kind: ActorSystem}
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
		Tag:     "harness",
		Actor:   &actor,
		Outcome: "failed",
	}, "high", true)
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
//
// `interrupt` is the deployment's own answer to "is this worth a buzz?". A
// notification that is not worth one is still delivered to a chat channel when
// one is configured — a chat message is read at the reader's convenience rather
// than competing with whatever they are doing — which is what keeps the summary
// of a short turn from disappearing entirely.
func (n *Notifier) notify(ctx context.Context, message Message, urgency string, interrupt bool) {
	if n.service != nil && interrupt {
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

// name returns the session's own name for a notification title.
//
// The title is the one field a lock screen always shows, so naming the session
// there is what makes two notifications from two sessions distinguishable at a
// glance. It is the same text IncludeSessionName governs, which is why the
// option's documentation applies to the body it actually controls; a deployment
// that wants none of it in either place should leave Describe unset.
func (n *Notifier) name(ctx context.Context, sessionID string) string {
	if n.describe == nil {
		return ""
	}
	return clip(n.describe(ctx, sessionID), 48)
}

// label returns the session's own name, or "" when naming it is not wanted.
//
// It is the gate on the operator's words reaching a notification *body*: every
// caller that would embed a title there goes through here, so turning
// IncludeSessionName off cannot leave one path still leaking it.
func (n *Notifier) label(ctx context.Context, sessionID string) string {
	if !n.includeName {
		return ""
	}
	return n.name(ctx, sessionID)
}

// conversationURL is where a tap lands: the app's own route for one session.
func conversationURL(sessionID string) string {
	return "./#/sessions/" + sessionID
}

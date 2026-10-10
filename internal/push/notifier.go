package push

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/questions"
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
// A delegated task's own finish is deliberately *not* on that list. A child
// agent settles inside its parent's turn, and the parent is still working when
// it does: announcing the child tells the operator that something finished while
// the thing they are waiting for has not. The turn's own notification is the one
// that answers "is it done?", and it carries the delegations in its summary, so
// nothing about the work is lost by waiting for it.
//
// Everything else — a message arriving while the reader watches, a session
// resync, a model catalog change — is silent by construction. The default case
// is silence rather than an empty branch, because the safe direction for an
// event type added later is not to wake anyone.
//
// What a notification *says* is the other half of the design, and this type no
// longer assembles it. A turn's own record — the message it ended on, and what
// it did — is stated by whoever owned the turn, in the frame that settles it
// (see events.TurnRecord). It used to be inferred here, from the messages and
// tool frames this process happened to see while the turn was live, and that is
// a description of the observer rather than of the turn: a gateway that attached
// fourteen minutes into a thirty-eight-minute turn reported fourteen minutes and
// the 105 calls it had watched, and — because DSH writes a turn's last message
// and its `turn/end` milliseconds apart — the one message a reader wants arrived
// one frame too late and was dropped, leaving the card to quote a narration from
// seven minutes earlier or a bare "6 tool calls". Nothing here keeps a ledger of
// a turn any more: the notifier decides whether a settlement is worth an
// interruption, and renders what the settlement states.
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
	// carryAnswers decides whether a notification carries the turn's closing
	// message. The text is on the settlement either way — it is the turn's own
	// record, and every client is shown the same message as a conversation row —
	// so this is a decision about what a *notification* may say, not about what
	// this process holds: see NotifierOptions.CarryAnswers.
	carryAnswers bool
	// describe names a session. Optional: without it the message still says
	// which actor settled and what it did, and says which session by id.
	describe func(ctx context.Context, sessionID string) string

	mu sync.Mutex
	// announced remembers the settlements already reported, so a settle frame
	// that arrives twice — a replayed stream, a watcher restarted onto the same
	// log — does not post the same card again. The card is a copy of the turn's
	// own words now, and a duplicate is a second copy of an answer rather than a
	// second "done" line.
	//
	// The key is the session and the turn together, because a turn id is only
	// unique within its session: the log watcher mints `log-3` for every session
	// it follows.
	announced map[string]struct{}
	// order bounds announced by first arrival, so a long-lived gateway cannot
	// accumulate one key per turn it has ever seen.
	order []string

	// ready is closed once the notifier is subscribed. A caller may wait on it
	// to know that an approval arriving this instant will not be missed, which
	// is also what a test needs in order not to race the goroutine it started.
	ready     chan struct{}
	readyOnce sync.Once
}

// maxAnnounced bounds the settlement ledger.
//
// It is a set of small keys rather than a ledger of turns — nothing but the fact
// that a turn was announced — and it is bounded because a gateway that runs for
// months would otherwise remember every turn it has ever reported.
const maxAnnounced = 1024

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
	//
	// It does not govern the message's *title*, which names the session whatever
	// this says: the title is the one field a lock screen always draws, and a
	// notification that cannot say which conversation it is about is a
	// notification the reader has to open to identify.
	IncludeSessionName bool
	// CarryAnswers allows a notification to carry the settlement's closing
	// message, so that a chat channel can print it.
	//
	// Set it when at least one webhook has answers enabled. It is a gate on what
	// leaves this process rather than on what it holds: the text arrives on the
	// settlement frame either way, because it is part of the turn's own record
	// and the same message is already on the client's stream as a conversation
	// row. What this decides is whether a *notification* — a lock screen, a
	// third-party chat service — is told it, and the lock-screen payload has no
	// field for it at all (see encrypt.go). Webhook.IncludeAnswer decides which
	// groups are shown it; a channel cannot print what the deployment never
	// allowed to travel.
	CarryAnswers bool
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
		bus:          opts.Bus,
		service:      opts.Service,
		webhooks:     opts.Webhooks,
		logger:       opts.Logger,
		threshold:    opts.Threshold,
		now:          opts.Now,
		describe:     opts.Describe,
		includeName:  opts.IncludeSessionName,
		carryAnswers: opts.CarryAnswers,
		announced:    map[string]struct{}{},
		ready:        make(chan struct{}),
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
	case events.TypeApprovalRequested:
		n.handleApproval(ctx, event)
	case events.TypeApprovalResolved:
		n.handleApprovalResolved(ctx, event)
	case events.TypeQuestionRequested:
		n.handleQuestion(ctx, event)
	case events.TypeQuestionResolved:
		n.handleQuestionResolved(ctx, event)
	case events.TypeHarnessState:
		n.handleHarness(ctx, event)
	default:
		// The frame types this switch does not name are silent on purpose. See
		// the policy in the type comment above. `session.message` and
		// `session.tool` are among them now: a turn's own words and its work
		// arrive on the settlement that states them, so nothing here watches a
		// turn go by.
	}
}

// turnStart decides when a turn began, for the purpose of "was it long enough
// to interrupt someone about".
//
// The payload's own StartedAt is believed when it is present, and that is the
// whole answer now: the party that owned the turn states when it began, from the
// scheduler's own bookkeeping or from the log's `turn/start`, so "the moment I
// first saw it running" is not needed. It was, once — the notifier is a late
// observer by nature, a subscriber that may attach at any point during a turn,
// and a watcher that could not say when a turn began left it reporting the wait
// it had witnessed as though it were the turn's: a thirty-eight-minute turn
// announced as fourteen.
//
// The fallback is for a producer that states no start at all: a zero duration is
// then reported as silence rather than as "0s", which is the honest reading of a
// fact nobody provided.
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
	if state.Subagent {
		// A delegated child's turn. It settles while the session that asked for
		// it is still working, and it is not a session the operator opened or
		// can be waiting on — the parent's own turn is. Announcing it would be
		// the notification policy's worst case: an interruption that says work
		// is finished when the work being waited for is not.
		//
		// Its record is not read either, and that is not an omission: the record
		// is quoted under the session it belongs to, and the session a reader is
		// waiting on is the parent.
		return
	}
	switch state.State {
	case "completed", "cancelled", "failed":
		// The states worth announcing, and the only ones. A queued or running
		// frame needs nothing kept here, because the settlement will state
		// everything a consumer could want to know about the turn — see
		// events.TurnRecord — so no ledger is opened when a turn starts.
	default:
		return
	}
	if !n.rememberAnnounced(event.SessionID, state.TurnID) {
		// A settle frame can arrive twice — a replayed stream, a restarted
		// watcher re-reading the same log — and the second one must not post the
		// same card again: the card is now a copy of the turn's own words, so a
		// duplicate is a second copy of an answer rather than a second "done".
		return
	}
	duration := n.now().Sub(turnStart(state, n.now))
	// A failure interrupts whatever its length. Everything else waits for the
	// threshold, because a short successful answer is something the operator is
	// still looking at — but a failure is the one outcome that leaves nothing
	// behind to come back to, and a prompt sent from a phone is exactly the kind
	// that is sent before walking away.
	interrupt := state.State == "failed" || duration >= n.threshold
	n.notify(ctx, n.turnMessage(ctx, event.SessionID, state, duration), turnUrgency(state.State), interrupt)
}

// rememberAnnounced records a settlement as reported, and answers whether it is
// news. An empty turn id cannot be recognized twice — the producers that matter
// all name one — so such a frame is always announced.
func (n *Notifier) rememberAnnounced(sessionID, turnID string) bool {
	if turnID == "" {
		return true
	}
	key := sessionID + "\x00" + turnID
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, seen := n.announced[key]; seen {
		return false
	}
	n.announced[key] = struct{}{}
	n.order = append(n.order, key)
	for len(n.order) > maxAnnounced {
		oldest := n.order[0]
		n.order = n.order[1:]
		delete(n.announced, oldest)
	}
	return true
}

// turnMessage states one settled turn: which conversation, what came of it, and
// what it said.
//
// The title is the session's own name, and it is the whole title. It used to be
// a sentence — "completed · Main agent · <name>" — which spent the largest text
// a chat client draws on two words the reader already knew, and left the one
// fact that tells two notifications apart competing for the rest of the line.
// The outcome and the actor are still there; they are the status line and the
// footnote the card draws under it, where a reader scans them rather than reads
// them.
//
// The summary and the answer are both quoted from the settlement's record — what
// the turn did, by its owner's count, and the last thing it said. Neither is
// derived here, which is the change this exists for: a notification used to be
// assembled from the fragments this process happened to see, so it described the
// observer. See events.TurnRecord.
func (n *Notifier) turnMessage(ctx context.Context, sessionID string, state events.TurnState, duration time.Duration) Message {
	actor := Actor{Kind: ActorMain}
	summary := workSummary(state.Record.Work)
	title := n.headline(ctx, sessionID, state)
	body := activityBody(summary, n.settledDetail(state))
	if body == "" {
		// Nothing was counted and nothing was explained — a question answered in
		// prose. The answer is the body of the card; this line is the lock
		// screen's, and there it says what the notification is for rather than
		// repeating the title, which is already the session's name.
		body = "Open the session for the result."
		if state.State == "failed" {
			body = "Open the session for the error."
		}
	}
	return Message{
		Title:      title,
		Body:       body,
		Answer:     n.closingText(state),
		URL:        conversationURL(sessionID),
		Tag:        "turn-" + sessionID,
		SessionID:  sessionID,
		Actor:      &actor,
		Summary:    summary,
		Model:      closingModel(state),
		Outcome:    outcomeWord(state.State),
		DurationMS: millis(duration),
	}
}

// closingText is what the turn said last, when this deployment sends that to a
// notification.
//
// Empty for a cancelled turn's *result*, but not for its words: a stop is a
// statement about the work, and what the agent had said before it stopped is
// still the most useful thing the card can carry. Empty when the turn ended on a
// tool call with nothing said after it, which is where the summary takes over.
func (n *Notifier) closingText(state events.TurnState) string {
	if !n.carryAnswers || state.Record.Closing == nil {
		return ""
	}
	return strings.TrimSpace(state.Record.Closing.Text)
}

// closingModel names the model that wrote the closing message, when a producer
// knew. It travels with the words rather than beside them: a card that does not
// carry an answer has no author to name, and the ACP stream does not report one
// on a committed message, so this is empty for a turn this gateway drove.
func closingModel(state events.TurnState) string {
	if state.Record.Closing == nil {
		return ""
	}
	return state.Record.Closing.Model
}

// headline names the thing this notification is about.
//
// The session's own name when there is one, because "which conversation?" is the
// question a reader asks before any other, and the title is the field a lock
// screen and a chat card both draw largest. Without a name — a deployment with
// no history to read, or an event that belongs to no session — it falls back to
// the sentence this used to be: the outcome and the actor, which is still more
// useful than an empty title.
func (n *Notifier) headline(ctx context.Context, sessionID string, state events.TurnState) string {
	if name := n.name(ctx, sessionID); name != "" {
		return name
	}
	return activityTitle(state.State, Actor{Kind: ActorMain}, "")
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

// handleQuestion reports an agent that has stopped to ask something.
//
// It interrupts for the same reason an approval does: the agent is blocked, and
// the only thing that unblocks it is a person looking at the card. What it
// deliberately does *not* carry is the question itself, and that is a privacy
// decision rather than a formatting one. A question is text a model wrote — it
// may name a file, a customer, a symptom — and the two places a notification
// goes are the lock screen and, if one is configured, a chat webhook. Neither is
// where this project puts model output; PRIVACY.md says so, and an earlier draft
// of this handler said otherwise. What is left is what the policy allows: the
// session's label, and a count.
func (n *Notifier) handleQuestion(ctx context.Context, event events.Event) {
	view, ok := event.Data.(questions.View)
	if !ok {
		n.mismatch(event, "questions.View")
		return
	}
	actor := Actor{Kind: ActorMain}
	body := "The agent is waiting for your answer."
	if label := n.label(ctx, event.SessionID); label != "" {
		body = fmt.Sprintf("%s · %s", body, label)
	}
	n.notify(ctx, Message{
		Title: "The agent is asking",
		Body:  body,
		URL:   conversationURL(event.SessionID),
		// One question replaces the last notification for the same one, and a
		// second question in another session does not stack behind it.
		Tag:       "question-" + view.ID,
		SessionID: event.SessionID,
		Actor:     &actor,
		Outcome:   "waiting",
		Summary:   questionCount(len(view.Items)),
	}, "high", true)
}

// handleQuestionResolved reports a question that ran out of time.
//
// A question a person answered is not news: they were holding the phone. A
// withdrawal is, and only one kind of it: a stopped turn was stopped by the
// operator, and a redeploy re-asks — but a question that expired was one nobody
// ever saw, and the model has by then carried on with an assumption of its own.
// That is exactly the case the product must not lose, because nothing else in
// the conversation says it happened.
func (n *Notifier) handleQuestionResolved(ctx context.Context, event events.Event) {
	decision, ok := event.Data.(questions.Decision)
	if !ok {
		n.mismatch(event, "questions.Decision")
		return
	}
	if decision.AnsweredBy != questions.ByTimeout {
		return
	}
	actor := Actor{Kind: ActorSystem}
	body := "The agent gave up waiting and carried on without an answer."
	if label := n.label(ctx, event.SessionID); label != "" {
		body = fmt.Sprintf("%s · %s", body, label)
	}
	n.notify(ctx, Message{
		Title:     "Question expired",
		Body:      body,
		URL:       conversationURL(event.SessionID),
		Tag:       "question-" + decision.ID,
		SessionID: event.SessionID,
		Actor:     &actor,
		Outcome:   "expired",
		Summary:   "unanswered",
	}, "high", true)
}

// questionCount phrases how much is waiting, which is all a notification may say
// about a question: see handleQuestion.
func questionCount(items int) string {
	if items <= 1 {
		return "1 question"
	}
	return fmt.Sprintf("%d questions", items)
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

// millis turns a duration into the wire's field.
//
// A zero or negative duration is reported as zero, which every reader treats as
// "unknown": a turn this process attached to mid-flight has no start, and the
// fallback start it is given would turn that absence into a wrong number
// instead of no number.
func millis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

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
//
// The budget is generous on purpose. It is not there to shorten a name for
// display — a chat client wraps its own title, and it is better at it than this
// is — but to stop a session whose "name" is the first line of a pasted
// paragraph from becoming a card that is nothing but that paragraph. It sits
// above every title the gateway has ever produced, so in practice the name is
// shown whole.
func (n *Notifier) name(ctx context.Context, sessionID string) string {
	if n.describe == nil {
		return ""
	}
	return clip(n.describe(ctx, sessionID), maxHeadlineRunes)
}

// maxHeadlineRunes bounds a session name used as a notification title.
const maxHeadlineRunes = 120

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

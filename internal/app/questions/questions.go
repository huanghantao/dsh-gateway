// Package questions brokers the agent's questions to a human.
//
// DeepSeek Harness lets the model stop mid-turn and ask the operator something —
// `ask_user_question`, whose card offers numbered choices, an optional free-text
// answer, and several questions in one request. Over ACP that capability does not
// exist: the bridge projects approvals to its client and nothing else, so the
// gateway supplies its own answerer (see internal/dshplugin) and this package is
// the other end of it, between that answerer and a phone screen.
//
// The shape of the wait is deliberately the approval broker's — park the request,
// announce it, let the caller block — because the two are the same problem: an
// agent that cannot proceed until a person answers. What differs is what silence
// means. An approval nobody answers is **refused**, because the safe reading of
// "no" is "do not run that tool". A question nobody answers is simply
// **unanswered**: there is nothing to refuse, the model is told so plainly, and
// it decides what to do next. Nothing here fails closed, and the timeout is a
// withdrawal rather than a decision.
//
// Two properties make the transport above this package survivable, and neither
// is an optimisation:
//
//   - A request id is idempotent. The answerer mints it once per question and
//     reuses it across retries, so a question that outlives the gateway process —
//     a redeploy in the middle of an unanswered question — is re-announced under
//     the same id instead of becoming a second card for one decision.
//   - A settled request is remembered briefly. Without that, a connection lost
//     between "the human answered" and "the answer was read" would ask the
//     operator the same question again.
package questions

import (
	"context"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Option is one choice the agent offered.
type Option struct {
	// Label is the choice exactly as the model wrote it, and is what an answer
	// must quote back: the model matches its own labels, so the gateway never
	// rewrites one. The harness's "(Recommended)" convention lives inside the
	// label, and Recommended below is the derived flag that lets a client draw a
	// badge instead of printing the suffix.
	Label string `json:"label"`
	// Description is one sentence of trade-off, when the model supplied one.
	Description string `json:"description,omitempty"`
	// Recommended reports that Label ends with the harness's recommendation
	// suffix. It is presentation only: the label keeps its suffix on the wire,
	// because that is the string the model will match.
	Recommended bool `json:"recommended,omitempty"`
}

// Item is one question in a request.
type Item struct {
	// ID is the model's stable identifier for the question, echoed in the answer
	// so a multi-question request can be matched up without relying on order.
	ID string `json:"id"`
	// Header is a short heading, such as "Confirm" or "回答风格".
	Header string `json:"header,omitempty"`
	// Question is the question itself.
	Question string `json:"question"`
	// Detail is supporting context that is not one of the choices: a plan under
	// review, a diff, the text of a decision. It renders with the question
	// rather than as an option.
	Detail string `json:"detail,omitempty"`
	// Options are the choices, and may be empty: a model that needs something
	// typed rather than chosen asks a question with no options at all.
	Options []Option `json:"options,omitempty"`
	// MultiSelect allows more than one label to come back.
	MultiSelect bool `json:"multiSelect,omitempty"`
}

// Answer is one question's answer.
//
// The two fields are not alternatives. For a single-select question Custom
// overrides Selected; for a multi-select question Custom supplements it. Both may
// be empty, which is how a skipped question is represented — the shape DSH's own
// documentation keeps for a question the operator passed over, and the reason a
// client can answer four questions on four cards without inventing a fifth
// "no answer" state.
type Answer struct {
	ID       string   `json:"id"`
	Selected []string `json:"selected"`
	Custom   string   `json:"custom,omitempty"`
}

// View is a pending question as the wire carries it.
type View struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	// Items is the request's questions in the order the model asked them. A
	// client pages through them; the order is the model's, not the gateway's.
	Items       []Item    `json:"items"`
	RequestedAt time.Time `json:"requestedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

// Decision is the payload of a resolved question.
//
// AnsweredBy carries most of the meaning: a device id means a person answered,
// while ByTimeout, ByCancelled and ByShutdown mean the question ended without
// one. A client that rendered those the same way would tell the operator their
// agent got an answer it never got.
type Decision struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId,omitempty"`
	// AnsweredBy is the device that answered, or the reason nobody did.
	AnsweredBy string `json:"answeredBy"`
	// Answers is empty unless a person answered.
	Answers []Answer `json:"answers,omitempty"`
}

// Reasons a question ended without an answer. They travel as AnsweredBy so that
// a client, a log line and a push notification describe the ending the same way.
const (
	// ByTimeout means the question outlived its window.
	ByTimeout = "timeout"
	// ByCancelled means the asker went away: the turn was stopped, or the child
	// that asked died. It is separate from ByTimeout because "you stopped it" and
	// "you never saw it" are different stories.
	ByCancelled = "cancelled"
	// ByShutdown means the gateway is going away and took the question with it.
	ByShutdown = "shutdown"
)

// Outcome is how a Request ended.
type Outcome string

const (
	// OutcomeAnswered means a person answered, possibly by skipping questions:
	// the answer set is complete and well formed either way.
	OutcomeAnswered Outcome = "answered"
	// OutcomeUnanswered means nobody answered, and Reason says why.
	OutcomeUnanswered Outcome = "unanswered"
)

// Request is one question arriving from the answerer.
type Request struct {
	// ID is the answerer's id for this ask, stable across its retries.
	ID string
	// SessionID is the DSH session that asked, which is also the agent's id.
	SessionID string
	// Items are the questions.
	Items []Item
}

// Result is what a Request settled as.
type Result struct {
	Outcome Outcome
	// Answers is complete whenever Outcome is OutcomeAnswered: one entry per
	// requested item, in the request's order, with skipped items present and
	// empty. A model reading the tool result should never have to guess which
	// questions went unanswered.
	Answers []Answer
	// Reason explains OutcomeUnanswered, and is empty otherwise.
	Reason string
}

// Broker parks questions until a human answers them.
type Broker struct {
	timeout time.Duration
	// settledTTL is how long a resolved question stays replayable, so a retry
	// arriving after the decision reads the decision rather than asking a person
	// a second time.
	settledTTL time.Duration
	now        func() time.Time
	logger     *logx.Logger
	bus        *events.Bus

	mu      sync.Mutex
	pending map[string]*pending
	settled map[string]settledEntry
	// order lists settled ids oldest first, so the cap has an oldest to drop
	// without scanning the map.
	order  []string
	closed bool
}

// pending is one announced question and everyone waiting on it.
type pending struct {
	view View
	// waiters are the callers blocked on this question. There is normally one —
	// the answerer's HTTP request — and a retry that overlaps the first attempt
	// is the only way a second appears. All of them are answered together: a
	// question is asked once and answered once, however many readers it has.
	waiters []chan Result
}

// settledEntry is a resolved question kept for replay.
type settledEntry struct {
	result     Result
	answeredBy string
	at         time.Time
}

// Options configures a Broker.
type Options struct {
	// Timeout bounds how long a question waits for a person. Zero takes the
	// default: a question is read and typed rather than tapped, so it gets
	// longer than an approval does.
	Timeout time.Duration
	// SettledTTL bounds the replay window for a resolved question. Zero takes
	// the default; a negative value disables replay, which only a test wants.
	SettledTTL time.Duration
	Bus        *events.Bus
	Logger     *logx.Logger
	Now        func() time.Time
}

const (
	// DefaultTimeout is how long a question waits when nothing configures one.
	DefaultTimeout = 10 * time.Minute
	// DefaultSettledTTL is how long a resolved question stays replayable.
	DefaultSettledTTL = 2 * time.Minute
	// maxSettled bounds the replay table. Each entry is a question and its
	// answers, so a long-lived gateway that answered thousands of questions
	// would otherwise hold every one of them; a TTL alone cannot bound a burst.
	maxSettled = 64
)

// New builds a Broker.
func New(opts Options) *Broker {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.SettledTTL == 0 {
		opts.SettledTTL = DefaultSettledTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = logx.Discard()
	}
	return &Broker{
		timeout:    opts.Timeout,
		settledTTL: opts.SettledTTL,
		now:        opts.Now,
		logger:     opts.Logger,
		bus:        opts.Bus,
		pending:    map[string]*pending{},
		settled:    map[string]settledEntry{},
	}
}

// Timeout is the window a question is given.
func (b *Broker) Timeout() time.Duration { return b.timeout }

// Request announces a question and blocks until it is answered, withdrawn, or
// the caller's context ends.
//
// The context is the answerer's HTTP request, so its cancellation is meaningful:
// it means the asker is gone — the turn was stopped, or the DSH child died — and
// the question is withdrawn rather than left on a phone for an agent that is no
// longer waiting.
func (b *Broker) Request(ctx context.Context, req Request) (Result, error) {
	view, err := b.buildView(req)
	if err != nil {
		return Result{}, err
	}

	answer := make(chan Result, 1)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return Result{}, errx.New(errx.KindUnavailable, "questions_closed",
			"the gateway is shutting down")
	}
	// A retry of a question that was already decided reads the decision. This is
	// what stops a lost connection from asking the operator the same thing twice.
	if entry, ok := b.settledWithinLocked(req.ID); ok {
		b.mu.Unlock()
		return entry.result, nil
	}
	p, known := b.pending[view.ID]
	if known {
		// A second reader for a question that is already on screen: same view,
		// same answer, and nothing announced again.
		p.waiters = append(p.waiters, answer)
		view = p.view
		b.mu.Unlock()
	} else {
		p = &pending{view: view, waiters: []chan Result{answer}}
		b.pending[view.ID] = p
		b.mu.Unlock()
		b.publish(events.TypeQuestionRequested, view)
		b.logger.Info("question asked",
			"question", view.ID, "session", view.SessionID, "items", len(view.Items))
	}

	// The wait is bounded twice, and neither bound substitutes for the other.
	// The context covers the asker going away; the timer covers a silent phone.
	// Without the timer a question nobody answers would never resolve at all —
	// the card would stay in every snapshot, the model would stay blocked, and
	// this goroutine would sit here until the socket dropped.
	wait := view.ExpiresAt.Sub(b.now())
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case result := <-answer:
		return result, nil
	case <-timer.C:
		return b.end(view.ID, Result{Outcome: OutcomeUnanswered, Reason: ByTimeout}, ByTimeout), nil
	case <-ctx.Done():
		return b.end(view.ID, Result{Outcome: OutcomeUnanswered, Reason: ByCancelled}, ByCancelled), nil
	}
}

// Answer resolves a pending question.
//
// Every answer must name a question the request carried, and every selected
// label must be one that question offered. Accepting an arbitrary string would
// let a client put words in the model's mouth: the model matches its own labels,
// so a label it never wrote is not an answer, it is noise the agent would try to
// reconcile.
//
// A question the client left out is filled in as skipped rather than rejected.
// The UI pages through questions, and an operator who answered the second of four
// and submitted has answered what they meant to.
func (b *Broker) Answer(id string, answers []Answer, answeredBy string) error {
	b.mu.Lock()
	p, ok := b.pending[id]
	if !ok {
		_, replayable := b.settledWithinLocked(id)
		b.mu.Unlock()
		if replayable {
			return errx.New(errx.KindConflict, "question_answered",
				"that question has already been answered")
		}
		return errx.New(errx.KindConflict, "question_closed",
			"that question is no longer pending; it was answered or it expired")
	}
	normalized, err := normalizeAnswers(p.view.Items, answers)
	if err != nil {
		b.mu.Unlock()
		return err
	}
	delete(b.pending, id)
	b.mu.Unlock()

	if answeredBy == "" {
		answeredBy = "operator"
	}
	b.finish(p, Result{Outcome: OutcomeAnswered, Answers: normalized}, answeredBy)
	b.logger.Info("question answered",
		"question", id, "session", p.view.SessionID, "by", answeredBy)
	return nil
}

// List returns pending questions, oldest first so the most urgent decision is at
// the top of a client's list.
func (b *Broker) List() []View {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]View, 0, len(b.pending))
	for _, p := range b.pending {
		out = append(out, p.view)
	}
	// Insertion sort: the pending set is the size of a conversation's open
	// questions, which is a handful, and this keeps the package free of a sort
	// dependency for a handful of elements.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].RequestedAt.Before(out[j-1].RequestedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Get returns one pending question.
func (b *Broker) Get(id string) (View, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[id]
	if !ok {
		return View{}, false
	}
	return p.view, true
}

// Close withdraws every pending question.
//
// It runs during shutdown so that no model is left waiting on a gateway that is
// going away. The questions are withdrawn rather than answered: the answerer is
// still connected and reports the withdrawal to the model, which is the honest
// outcome — nobody decided anything.
func (b *Broker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	open := make([]*pending, 0, len(b.pending))
	for _, p := range b.pending {
		open = append(open, p)
	}
	b.pending = map[string]*pending{}
	b.mu.Unlock()

	for _, p := range open {
		b.finish(p, Result{Outcome: OutcomeUnanswered, Reason: ByShutdown}, ByShutdown)
	}
}

// end settles a question that is being withdrawn, and reports what actually
// happened.
//
// A timeout and an answer can race, and so can a cancellation and a shutdown.
// Only one of them removes the question; the loser asks this for the outcome
// instead of announcing its own, so no reader is told "nobody answered" about a
// question a person did answer.
func (b *Broker) end(id string, outcome Result, reason string) Result {
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	entry, settled := b.settledWithinLocked(id)
	b.mu.Unlock()

	if !ok {
		if settled {
			return entry.result
		}
		return outcome
	}
	b.finish(p, outcome, reason)
	return outcome
}

// finish remembers a question's outcome, wakes everyone waiting on it, and
// announces it. It is the single path that ends a question, so there is exactly
// one place where what a client sees and what the answerer receives can drift.
func (b *Broker) finish(p *pending, result Result, answeredBy string) {
	b.remember(p.view.ID, result, answeredBy)

	if answeredBy == "" {
		answeredBy = "operator"
	}
	b.publish(events.TypeQuestionResolved, Decision{
		ID:         p.view.ID,
		SessionID:  p.view.SessionID,
		AnsweredBy: answeredBy,
		Answers:    result.Answers,
	})
	if result.Outcome == OutcomeUnanswered {
		b.logger.Warn("question ended without an answer",
			"question", p.view.ID, "session", p.view.SessionID, "reason", result.Reason)
	}

	// Buffered with capacity one and written exactly once, so this never blocks.
	for _, ch := range p.waiters {
		select {
		case ch <- result:
		default:
		}
	}
}

// remember keeps a resolved question replayable for a short while.
func (b *Broker) remember(id string, result Result, answeredBy string) {
	if b.settledTTL <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.settled[id]; !exists {
		b.order = append(b.order, id)
	}
	b.settled[id] = settledEntry{result: result, answeredBy: answeredBy, at: b.now()}

	for len(b.order) > maxSettled {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.settled, oldest)
	}
}

// settledWithinLocked reports whether a question was decided inside the replay
// window. The caller holds b.mu.
func (b *Broker) settledWithinLocked(id string) (settledEntry, bool) {
	entry, ok := b.settled[id]
	if !ok || b.now().Sub(entry.at) >= b.settledTTL {
		return settledEntry{}, false
	}
	return entry, true
}

// buildView validates a request and renders it for the wire.
func (b *Broker) buildView(req Request) (View, error) {
	if req.ID == "" {
		return View{}, errx.New(errx.KindInvalid, "question_id_required",
			"a question request needs an id")
	}
	if len(req.Items) == 0 {
		return View{}, errx.New(errx.KindInvalid, "question_items_required",
			"a question request needs at least one question")
	}

	seen := make(map[string]struct{}, len(req.Items))
	items := make([]Item, 0, len(req.Items))
	for _, item := range req.Items {
		if item.ID == "" {
			return View{}, errx.New(errx.KindInvalid, "question_item_id_required",
				"every question needs an id, because the answer echoes it back")
		}
		if _, dup := seen[item.ID]; dup {
			return View{}, errx.New(errx.KindInvalid, "duplicate_question_id",
				"two questions in one request share the id "+item.ID)
		}
		seen[item.ID] = struct{}{}
		if item.Question == "" {
			return View{}, errx.New(errx.KindInvalid, "question_text_required",
				"every question needs text to show the operator")
		}
		for _, option := range item.Options {
			if option.Label == "" {
				return View{}, errx.New(errx.KindInvalid, "option_label_required",
					"every option needs a label: the label is what an answer quotes back")
			}
		}
		items = append(items, item)
	}

	now := b.now()
	return View{
		ID:          req.ID,
		SessionID:   req.SessionID,
		Items:       items,
		RequestedAt: now,
		ExpiresAt:   now.Add(b.timeout),
	}, nil
}

// normalizeAnswers checks an answer set and completes it.
func normalizeAnswers(items []Item, answers []Answer) ([]Answer, error) {
	byID := make(map[string]Answer, len(answers))
	for _, answer := range answers {
		if _, dup := byID[answer.ID]; dup {
			return nil, errx.New(errx.KindInvalid, "duplicate_answer",
				"two answers name the question "+answer.ID)
		}
		byID[answer.ID] = answer
	}
	for id := range byID {
		if !hasItem(items, id) {
			return nil, errx.New(errx.KindInvalid, "unknown_question",
				"that answer names a question this request did not ask: "+id)
		}
	}

	out := make([]Answer, 0, len(items))
	for _, item := range items {
		answer, ok := byID[item.ID]
		if !ok {
			// Unanswered in the submitted set. Kept as an explicit skip so the
			// model sees a complete answer set rather than a short one.
			out = append(out, Answer{ID: item.ID, Selected: []string{}})
			continue
		}
		if !item.MultiSelect && len(answer.Selected) > 1 {
			return nil, errx.New(errx.KindInvalid, "too_many_selections",
				"question "+item.ID+" accepts a single choice")
		}
		for _, label := range answer.Selected {
			if !offers(item.Options, label) {
				return nil, errx.New(errx.KindInvalid, "unknown_option",
					"question "+item.ID+" was not offered the choice "+label)
			}
		}
		selected := answer.Selected
		if selected == nil {
			selected = []string{}
		}
		out = append(out, Answer{ID: item.ID, Selected: selected, Custom: answer.Custom})
	}
	return out, nil
}

func offers(options []Option, label string) bool {
	for _, option := range options {
		if option.Label == label {
			return true
		}
	}
	return false
}

func hasItem(items []Item, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// publish sends an event when there is a bus. A broker without one is a test
// that cares about parking and answering, not about what the wire saw.
func (b *Broker) publish(t events.Type, data any) {
	if b.bus == nil {
		return
	}
	sessionID := ""
	switch payload := data.(type) {
	case View:
		sessionID = payload.SessionID
	case Decision:
		sessionID = payload.SessionID
	}
	b.bus.Publish(t, sessionID, data)
}

package questions_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/questions"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

/* --------------------------------------------------------------- fixtures */

// clock is a hand-wound clock, so an expiry is something a test asserts rather
// than waits for.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// bench is a broker plus the bus it publishes on, wired the way production does.
type bench struct {
	broker *questions.Broker
	bus    *events.Bus
	clock  *clock
	sub    *events.Subscription
}

func newBench(t *testing.T, timeout time.Duration) *bench {
	t.Helper()
	bus := events.New(events.Config{Replay: 128, Queue: 128})
	clk := newClock()
	broker := questions.New(questions.Options{
		Timeout: timeout,
		Bus:     bus,
		Logger:  logx.Discard(),
		Now:     clk.now,
	})
	t.Cleanup(broker.Close)
	sub := bus.Subscribe(events.Cursor{Generation: bus.Generation()}, nil).Subscription
	t.Cleanup(sub.Close)
	return &bench{broker: broker, bus: bus, clock: clk, sub: sub}
}

// awaitEvent waits for one event of a type, so a test never sleeps on a race it
// could observe directly.
func (b *bench) awaitEvent(t *testing.T, want events.Type) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-b.sub.Events():
			if e.Type == want {
				return e
			}
		case <-deadline:
			t.Fatalf("no %s event arrived", want)
		}
	}
}

// asks is the request every test starts from: two questions, one of them a
// single choice with a recommended option and one multi-select.
func asks(id string) questions.Request {
	return questions.Request{
		ID:        id,
		SessionID: "session-1",
		Items: []questions.Item{
			{
				ID:       "style",
				Header:   "回答风格",
				Question: "你希望我平时回答的风格是？",
				Options: []questions.Option{
					{Label: "简洁直接，结论先行 (Recommended)", Description: "先给结论。", Recommended: true},
					{Label: "详细解释", Description: "把推导讲清楚。"},
				},
			},
			{
				ID:          "extras",
				Question:    "还要什么？",
				MultiSelect: true,
				Options:     []questions.Option{{Label: "代码"}, {Label: "命令"}},
			},
		},
	}
}

/* ------------------------------------------------------------------ tests */

func TestRequestBlocksUntilAnswered(t *testing.T) {
	b := newBench(t, time.Minute)

	type outcome struct {
		result questions.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := b.broker.Request(context.Background(), asks("q-1"))
		done <- outcome{result, err}
	}()

	requested := b.awaitEvent(t, events.TypeQuestionRequested)
	view, ok := requested.Data.(questions.View)
	if !ok {
		t.Fatalf("question.requested carried %T, want questions.View", requested.Data)
	}
	if view.ID != "q-1" || view.SessionID != "session-1" {
		t.Fatalf("event carried the wrong identity: %+v", view)
	}
	if requested.SessionID != "session-1" {
		t.Fatalf("event session = %q, want the asking session", requested.SessionID)
	}
	if !view.ExpiresAt.After(view.RequestedAt) {
		t.Fatalf("a question must carry a window: %+v", view)
	}
	if len(view.Items) != 2 || !view.Items[1].MultiSelect {
		t.Fatalf("the questions did not survive the round trip: %+v", view.Items)
	}

	if _, pending := b.broker.Get("q-1"); !pending {
		t.Fatal("the question is not listed as pending while it is being asked")
	}

	if err := b.broker.Answer("q-1", []questions.Answer{{
		ID:       "style",
		Selected: []string{"简洁直接，结论先行 (Recommended)"},
	}}, "device-7"); err != nil {
		t.Fatalf("answer: %v", err)
	}

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("request returned %v", got.err)
		}
		if got.result.Outcome != questions.OutcomeAnswered {
			t.Fatalf("outcome = %q, want answered", got.result.Outcome)
		}
		// The unsubmitted question is filled in as an explicit skip rather than
		// dropped: the model must see one answer per question it asked.
		if len(got.result.Answers) != 2 {
			t.Fatalf("answers = %+v, want one per question", got.result.Answers)
		}
		if got.result.Answers[1].ID != "extras" || len(got.result.Answers[1].Selected) != 0 {
			t.Fatalf("skipped question = %+v, want an empty selection", got.result.Answers[1])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the request never returned")
	}

	if _, pending := b.broker.Get("q-1"); pending {
		t.Fatal("an answered question is still pending")
	}
	decided := b.awaitEvent(t, events.TypeQuestionResolved)
	decision, ok := decided.Data.(questions.Decision)
	if !ok {
		t.Fatalf("question.resolved carried %T, want questions.Decision", decided.Data)
	}
	if decision.AnsweredBy != "device-7" {
		t.Fatalf("answeredBy = %q, want the device that answered", decision.AnsweredBy)
	}
}

func TestRequestThatNobodyAnswersIsWithdrawn(t *testing.T) {
	b := newBench(t, 30*time.Millisecond)

	result, err := b.broker.Request(context.Background(), asks("q-2"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if result.Outcome != questions.OutcomeUnanswered || result.Reason != questions.ByTimeout {
		t.Fatalf("result = %+v, want unanswered by timeout", result)
	}
	if len(result.Answers) != 0 {
		t.Fatalf("an unanswered question must carry no answers: %+v", result.Answers)
	}

	if _, pending := b.broker.Get("q-2"); pending {
		t.Fatal("an expired question is still pending")
	}
	decision := b.awaitEvent(t, events.TypeQuestionResolved).Data.(questions.Decision)
	if decision.AnsweredBy != questions.ByTimeout {
		t.Fatalf("answeredBy = %q, want %q", decision.AnsweredBy, questions.ByTimeout)
	}
}

func TestCancelledAskerWithdrawsTheQuestion(t *testing.T) {
	b := newBench(t, time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan questions.Result, 1)
	go func() {
		result, _ := b.broker.Request(ctx, asks("q-3"))
		done <- result
	}()
	b.awaitEvent(t, events.TypeQuestionRequested)

	// The turn was stopped, which the answerer reports by dropping its request.
	cancel()

	select {
	case result := <-done:
		if result.Outcome != questions.OutcomeUnanswered || result.Reason != questions.ByCancelled {
			t.Fatalf("result = %+v, want unanswered by cancellation", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling the asker did not settle the request")
	}
	if _, pending := b.broker.Get("q-3"); pending {
		t.Fatal("a withdrawn question is still pending")
	}
	if decision := b.awaitEvent(t, events.TypeQuestionResolved).Data.(questions.Decision); decision.AnsweredBy != questions.ByCancelled {
		t.Fatalf("answeredBy = %q, want %q", decision.AnsweredBy, questions.ByCancelled)
	}
}

// A retry is what a gateway redeploy looks like from the plugin's side: the
// connection died, so it asks again with the id it already minted.
func TestRetryOfAPendingQuestionDoesNotAskTwice(t *testing.T) {
	b := newBench(t, time.Minute)

	first := make(chan questions.Result, 1)
	second := make(chan questions.Result, 1)
	go func() {
		result, _ := b.broker.Request(context.Background(), asks("q-4"))
		first <- result
	}()
	b.awaitEvent(t, events.TypeQuestionRequested)
	go func() {
		result, _ := b.broker.Request(context.Background(), asks("q-4"))
		second <- result
	}()

	// One announcement, however many readers: the card must not stack up.
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case e := <-b.sub.Events():
			if e.Type == events.TypeQuestionRequested {
				t.Fatal("a retry announced the question a second time")
			}
		case <-deadline:
			goto answered
		}
	}

answered:
	if err := b.broker.Answer("q-4", []questions.Answer{{ID: "style", Selected: []string{"详细解释"}}}, "device-1"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	for name, ch := range map[string]chan questions.Result{"first": first, "second": second} {
		select {
		case result := <-ch:
			if result.Outcome != questions.OutcomeAnswered {
				t.Fatalf("%s reader got %+v, want the answer", name, result)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s reader was never woken", name)
		}
	}
}

// The answer decided before a connection dropped must be replayed, not asked
// again: the operator answered once.
func TestSettledQuestionIsReplayedToALateRetry(t *testing.T) {
	b := newBench(t, time.Minute)

	done := make(chan questions.Result, 1)
	go func() {
		result, _ := b.broker.Request(context.Background(), asks("q-5"))
		done <- result
	}()
	b.awaitEvent(t, events.TypeQuestionRequested)
	if err := b.broker.Answer("q-5", []questions.Answer{{ID: "style", Selected: []string{"详细解释"}}}, "device-1"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	<-done

	replayed, err := b.broker.Request(context.Background(), asks("q-5"))
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if replayed.Outcome != questions.OutcomeAnswered {
		t.Fatalf("replay = %+v, want the original answer", replayed)
	}
	if len(replayed.Answers) != 2 || replayed.Answers[0].Selected[0] != "详细解释" {
		t.Fatalf("replay lost the answer: %+v", replayed.Answers)
	}
}

func TestAnswerIsValidated(t *testing.T) {
	cases := []struct {
		name    string
		answers []questions.Answer
		code    string
	}{
		{
			name:    "an option the question never offered",
			answers: []questions.Answer{{ID: "style", Selected: []string{"多给代码"}}},
			code:    "unknown_option",
		},
		{
			name:    "a question this request did not ask",
			answers: []questions.Answer{{ID: "nope", Selected: nil}},
			code:    "unknown_question",
		},
		{
			name: "two labels for a single-choice question",
			answers: []questions.Answer{{
				ID:       "style",
				Selected: []string{"详细解释", "简洁直接，结论先行 (Recommended)"},
			}},
			code: "too_many_selections",
		},
		{
			name: "the same question answered twice",
			answers: []questions.Answer{
				{ID: "style", Selected: []string{"详细解释"}},
				{ID: "style", Selected: []string{"详细解释"}},
			},
			code: "duplicate_answer",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, time.Minute)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = b.broker.Request(context.Background(), asks("q-6"))
			}()
			b.awaitEvent(t, events.TypeQuestionRequested)

			err := b.broker.Answer("q-6", tc.answers, "device-1")
			if err == nil {
				t.Fatal("a malformed answer was accepted")
			}
			if got := errx.CodeOf(err); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
			// A rejected answer must leave the question answerable: the operator
			// is looking at the card and will try again.
			if _, pending := b.broker.Get("q-6"); !pending {
				t.Fatal("a rejected answer closed the question")
			}
			if err := b.broker.Answer("q-6", nil, "device-1"); err != nil {
				t.Fatalf("a skip-only answer must be accepted: %v", err)
			}
			<-done
		})
	}
}

func TestAnswerAfterTheQuestionEndedConflicts(t *testing.T) {
	b := newBench(t, time.Minute)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.broker.Request(context.Background(), asks("q-7"))
	}()
	b.awaitEvent(t, events.TypeQuestionRequested)
	if err := b.broker.Answer("q-7", nil, "device-1"); err != nil {
		t.Fatalf("first answer: %v", err)
	}
	<-done

	err := b.broker.Answer("q-7", nil, "device-2")
	if err == nil {
		t.Fatal("a second answer was accepted")
	}
	if got := errx.CodeOf(err); got != "question_answered" {
		t.Fatalf("code = %q, want question_answered", got)
	}
}

func TestCloseWithdrawsEverythingPending(t *testing.T) {
	b := newBench(t, time.Minute)

	done := make(chan questions.Result, 1)
	go func() {
		result, _ := b.broker.Request(context.Background(), asks("q-8"))
		done <- result
	}()
	b.awaitEvent(t, events.TypeQuestionRequested)

	b.broker.Close()

	select {
	case result := <-done:
		if result.Reason != questions.ByShutdown {
			t.Fatalf("reason = %q, want %q", result.Reason, questions.ByShutdown)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown left a request parked")
	}

	if _, err := b.broker.Request(context.Background(), asks("q-9")); err == nil {
		t.Fatal("a closed broker accepted a new question")
	} else if got := errx.CodeOf(err); got != "questions_closed" {
		t.Fatalf("code = %q, want questions_closed", got)
	}
}

func TestMalformedRequestsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		req  questions.Request
		code string
	}{
		{
			name: "no id",
			req:  questions.Request{Items: []questions.Item{{ID: "a", Question: "?"}}},
			code: "question_id_required",
		},
		{
			name: "no questions",
			req:  questions.Request{ID: "q"},
			code: "question_items_required",
		},
		{
			name: "a question with no id",
			req:  questions.Request{ID: "q", Items: []questions.Item{{Question: "?"}}},
			code: "question_item_id_required",
		},
		{
			name: "two questions sharing an id",
			req: questions.Request{ID: "q", Items: []questions.Item{
				{ID: "a", Question: "?"}, {ID: "a", Question: "?"},
			}},
			code: "duplicate_question_id",
		},
		{
			name: "a question with no text",
			req:  questions.Request{ID: "q", Items: []questions.Item{{ID: "a"}}},
			code: "question_text_required",
		},
		{
			name: "an option with no label",
			req: questions.Request{ID: "q", Items: []questions.Item{
				{ID: "a", Question: "?", Options: []questions.Option{{Description: "no label"}}},
			}},
			code: "option_label_required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBench(t, time.Minute)
			if _, err := b.broker.Request(context.Background(), tc.req); err == nil {
				t.Fatal("a malformed request was accepted")
			} else if got := errx.CodeOf(err); got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestListIsOldestFirst(t *testing.T) {
	b := newBench(t, time.Minute)

	for _, id := range []string{"first", "second"} {
		go func(id string) { _, _ = b.broker.Request(context.Background(), asks(id)) }(id)
		b.awaitEvent(t, events.TypeQuestionRequested)
		b.clock.advance(time.Second)
	}

	list := b.broker.List()
	if len(list) != 2 {
		t.Fatalf("list = %+v, want two pending questions", list)
	}
	if list[0].ID != "first" {
		t.Fatalf("list order = %s, %s; want the oldest first", list[0].ID, list[1].ID)
	}
}

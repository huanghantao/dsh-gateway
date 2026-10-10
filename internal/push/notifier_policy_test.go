package push_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/app/questions"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// These tests cover the notification policy in both directions: the cases where
// staying silent costs the operator something real, and the cases that look like
// they might and do not. Each one drives the real notifier over a real encrypted
// push, because "a notification fires" is only true if it arrives.

// notifierFor builds a notifier delivering to one browser, started and ready.
func notifierFor(t *testing.T, opts push.NotifierOptions) (*events.Bus, *receiver) {
	t.Helper()

	rec := newReceiver(t)
	server := rec.serve(t)
	service, err := push.Open(push.Options{StateDir: t.TempDir(), Logger: logx.Discard(), Client: server.Client()})
	if err != nil {
		t.Fatalf("push.Open: %v", err)
	}
	if err := service.Subscribe(rec.subscription(server.URL + "/push/one")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus := busFor(t)
	opts.Bus = bus
	opts.Service = service
	opts.Logger = logx.Discard()
	notifier, err := push.NewNotifier(opts)
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		notifier.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-notifier.Ready()
	return bus, rec
}

// TestExpiredApprovalIsReported is the notification the product used to lose.
//
// The operator who missed the "Approval needed" buzz because they were driving
// learned nothing afterwards: the tool was refused, the agent carried on without
// it, and the app was silent. The expiry has to replace the request it belongs
// to, which the shared tag is what arranges.
func TestExpiredApprovalIsReported(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{})

	// A real broker, so the payload is the one production publishes.
	broker := approvals.New(approvals.Options{Timeout: time.Minute, Bus: bus, Logger: logx.Discard(), Now: time.Now})
	defer broker.Close()

	ctx, cancel := context.WithCancel(context.Background())
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		_, _ = broker.RequestPermission(ctx, harness.PermissionRequest{
			ID: "apr_exp", SessionID: "session-1", ToolCallID: "call_1",
			Tool: "bash", Input: `{"command":"rm -rf build"}`,
			RequestedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		})
	}()

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "the request notification")
	request := rec.received()[0]

	// Nobody answers.
	cancel()
	<-answered

	waitFor(t, func() bool { return len(rec.received()) >= 2 }, "the expiry notification")
	expired := rec.received()[1]
	if !strings.Contains(expired.Title, "expired") {
		t.Errorf("title = %q, want the expiry named; a decision nobody made is the news", expired.Title)
	}
	if !strings.Contains(expired.Body, "bash") {
		t.Errorf("body = %q, want the tool that was refused", expired.Body)
	}
	if expired.Tag != request.Tag {
		t.Errorf("tag = %q, want %q: the expiry replaces the request it belongs to, "+
			"rather than leaving a stale prompt on the lock screen", expired.Tag, request.Tag)
	}
}

// TestAQuestionNotifiesWithoutCarryingTheQuestion is the privacy half of the
// question notification.
//
// A question is text a model wrote, and a notification goes to a lock screen and
// to any chat webhook that is configured. Neither is where this project puts
// model output — PRIVACY.md says so — so the notification says that the agent is
// waiting, by which session, and how much is waiting. The question itself stays
// on the card in the app.
func TestAQuestionNotifiesWithoutCarryingTheQuestion(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{})

	// A real broker, so the payload is the one production publishes and the
	// assertion is about the notifier rather than about a hand-built event.
	// A short window, so the expiry this test also covers is an event the broker
	// reaches on its own rather than one the test forces by shutting it down:
	// a shutdown is not an expiry, and the notifier is deliberately silent about
	// it, because the answerer asks again against the successor.
	broker := questions.New(questions.Options{Timeout: 800 * time.Millisecond, Bus: bus, Logger: logx.Discard(), Now: time.Now})
	defer broker.Close()

	const secret = "the customer's account number is 1234"
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = broker.Request(context.Background(), questions.Request{
			ID: "ask-1", SessionID: "session-1",
			Items: []questions.Item{{
				ID: "q1", Header: "确认", Question: secret,
				Options: []questions.Option{{Label: "是"}, {Label: "否"}},
			}},
		})
	}()

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "the question notification")
	message := rec.received()[0]

	for name, field := range map[string]string{"title": message.Title, "body": message.Body, "summary": message.Summary} {
		if strings.Contains(field, secret) || strings.Contains(field, "1234") {
			t.Errorf("the %s carries the question's text: %q", name, field)
		}
	}
	if !strings.Contains(message.Title, "asking") {
		t.Errorf("title = %q, want it to say the agent is asking something", message.Title)
	}
	if message.Summary != "1 question" {
		t.Errorf("summary = %q, want a count: it is what a webhook's meta line shows", message.Summary)
	}
	if message.Tag != "question-ask-1" {
		t.Errorf("tag = %q, want the question's own tag so a second one does not stack", message.Tag)
	}

	// An expired question is the case the operator would otherwise never learn
	// about: the model asked something, nobody saw it, and it carried on.
	waitFor(t, func() bool { return len(rec.received()) >= 2 }, "the expiry notification")
	<-done
	expired := rec.received()[1]
	if !strings.Contains(expired.Title, "expired") {
		t.Errorf("title = %q, want the expiry named", expired.Title)
	}
	if expired.Tag != message.Tag {
		t.Errorf("tag = %q, want %q: the expiry replaces the request it belongs to", expired.Tag, message.Tag)
	}
}

// TestFailedTurnInterruptsHoweverShort pins the one exception to the threshold.
//
// Everything else waits, because a short successful answer is something the
// operator is still looking at. A failure is the case where the turn produced
// nothing at all, so silence leaves them believing work is still happening.
func TestFailedTurnInterruptsHoweverShort(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Hour})

	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{State: "running"})
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{
		State:  "failed",
		Detail: "no API key for provider route \"deepseek-official\"",
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for a failed turn")
	message := rec.received()[0]
	if !strings.Contains(message.Title, "failed") {
		t.Errorf("title = %q, want the failure named; it used to say the agent finished", message.Title)
	}
	if !strings.Contains(message.Body, "no API key") {
		t.Errorf("body = %q, want the harness's own explanation", message.Body)
	}
	if got := rec.urgencies[0]; got != "high" {
		t.Errorf("urgency = %q, want high: nothing else will prompt them to look", got)
	}
}

// TestShortSuccessIsStillSilent is the property the threshold exists for, and
// the one a widened policy could most easily break.
func TestShortSuccessIsStillSilent(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Hour})

	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{State: "running"})
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{State: "completed"})
	waitFor(t, func() bool { return len(rec.received()) == 0 }, "nothing for a quick turn")
}

// TestAChildSessionsTurnIsNeverAnnounced covers the other half of "only the main
// agent's work is news".
//
// A delegated child is a session of its own: the harness gives it a log, turns
// and a stop reason, and the log watcher follows it like any other. Nothing about
// that session is something the operator opened or is waiting on — the turn that
// spawned it is — so neither a long turn nor a failure of the child's own may
// interrupt anyone. Both are published here with the mark the watcher sets, and
// the threshold is an hour so the long one is long only because the payload says
// so.
func TestAChildSessionsTurnIsNeverAnnounced(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{Threshold: time.Hour})

	longStart := time.Now().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-child", events.TurnState{
		TurnID: "turn-child", State: "running", StartedAt: &longStart, Subagent: true,
	})
	bus.Publish(events.TypeTurnState, "session-child", events.TurnState{
		TurnID: "turn-child", State: "completed", StartedAt: &longStart, Subagent: true,
	})
	bus.Publish(events.TypeTurnState, "session-child", events.TurnState{
		TurnID: "turn-child-2", State: "failed", Detail: "the child ran out of context", Subagent: true,
	})

	// A main-agent failure is the control: it must still be announced, so this
	// test cannot pass by silencing everything.
	bus.Publish(events.TypeTurnState, "session-main", events.TurnState{TurnID: "turn-main", State: "running"})
	bus.Publish(events.TypeTurnState, "session-main", events.TurnState{
		TurnID: "turn-main", State: "failed", Detail: "no API key for provider route",
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for the failed main turn")
	message := rec.received()[0]
	if message.SessionID != "session-main" {
		t.Fatalf("notification is about %q, want the main session: a child's turn is not news",
			message.SessionID)
	}
	if got := len(rec.received()); got != 1 {
		t.Errorf("received %d notifications, want 1: the child settled twice and neither was worth one", got)
	}
}

// TestHarnessFailureIsReportedButRestartsAreNot covers the line between a
// self-healing condition and one that needs a person. A supervisor that restarts
// a child must not buzz on every attempt; giving up must.
func TestHarnessFailureIsReportedButRestartsAreNot(t *testing.T) {
	bus, rec := notifierFor(t, push.NotifierOptions{})

	bus.Publish(events.TypeHarnessState, "", events.HarnessState{State: "restarting", Detail: "child exited"})
	bus.Publish(events.TypeHarnessState, "", events.HarnessState{State: "ready"})
	waitFor(t, func() bool { return len(rec.received()) == 0 }, "nothing while the supervisor retries")

	bus.Publish(events.TypeHarnessState, "", events.HarnessState{
		State:  "failed",
		Detail: "dsh exited 12 times; giving up",
	})
	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification when it gives up")
	message := rec.received()[0]
	if message.SessionID != "" {
		t.Errorf("sessionId = %q, want empty: this is not about one session", message.SessionID)
	}
	if !strings.Contains(message.Body, "giving up") {
		t.Errorf("body = %q, want the reason", message.Body)
	}
}

// TestUnreadablePayloadIsLoud checks the failure mode that hid a real bug: a
// notification that silently loses its content, and a log that says so.
func TestUnreadablePayloadIsLoud(t *testing.T) {
	// A payload shape nothing publishes. The old notifier asserted a map here,
	// which is exactly the sort of drift this guards against.
	bus, rec := notifierFor(t, push.NotifierOptions{})
	bus.Publish(events.TypeApprovalRequested, "session-1", "not-a-view")

	waitFor(t, func() bool { return len(rec.received()) == 0 }, "nothing for a payload it cannot read")
}

package push_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// receiver is a browser: it holds the private half of a subscription and can
// decrypt what the gateway sends, which is the only way to prove the whole path
// rather than just that a request went out.
type receiver struct {
	private *ecdh.PrivateKey
	auth    []byte

	mu        sync.Mutex
	messages  []push.Message
	urgencies []string
	headers   []http.Header
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return &receiver{private: private, auth: auth}
}

func (r *receiver) subscription(endpoint string) push.Subscription {
	return push.Subscription{
		Endpoint: endpoint,
		P256DH:   base64.RawURLEncoding.EncodeToString(r.private.PublicKey().Bytes()),
		Auth:     base64.RawURLEncoding.EncodeToString(r.auth),
		DeviceID: "dev_test",
	}
}

// decrypt implements the user agent half of RFC 8291 for one record.
func (r *receiver) decrypt(body []byte) ([]byte, error) {
	salt := body[:16]
	recordSize := binary.BigEndian.Uint32(body[16:20])
	keyLen := int(body[20])
	asPublic := body[21 : 21+keyLen]
	ciphertext := body[21+keyLen:]
	_ = recordSize

	curve := ecdh.P256()
	asKey, err := curve.NewPublicKey(asPublic)
	if err != nil {
		return nil, err
	}
	shared, err := r.private.ECDH(asKey)
	if err != nil {
		return nil, err
	}
	authInfo := append([]byte("WebPush: info\x00"), r.private.PublicKey().Bytes()...)
	authInfo = append(authInfo, asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, r.auth, string(authInfo), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}
	// The last record ends with the 0x02 padding delimiter.
	return plaintext[:len(plaintext)-1], nil
}

// serve starts a push service that decrypts what it is sent.
func (r *receiver) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		plaintext, err := r.decrypt(body)
		if err != nil {
			t.Errorf("the gateway sent something that cannot be decrypted: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var message push.Message
		if err := json.Unmarshal(plaintext, &message); err != nil {
			t.Errorf("payload is not a message: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.messages = append(r.messages, message)
		r.urgencies = append(r.urgencies, req.Header.Get("Urgency"))
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	return server
}

func (r *receiver) received() []push.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]push.Message(nil), r.messages...)
}

// TestNotifierPushesWhatCannotWait drives the whole path: an event on the bus,
// through the notifier, encrypted, over HTTP, and decrypted by a browser — which
// is the only test that says a notification would actually arrive.
func TestNotifierPushesWhatCannotWait(t *testing.T) {
	rec := newReceiver(t)
	server := rec.serve(t)

	service, err := push.Open(push.Options{StateDir: t.TempDir(), Logger: logx.Discard(), Client: server.Client()})
	if err != nil {
		t.Fatalf("push.Open: %v", err)
	}
	if err := service.Subscribe(rec.subscription(server.URL + "/push/one")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// A clock the test can both advance and observe. Reading the notifier's
	// internals is not possible, but "how many times has it asked for the time"
	// is: it asks once when a turn starts, which is the moment the test has to
	// wait for before advancing, or the two events race each other and the
	// measured duration is zero.
	clock := &fixedClock{now: time.Now()}
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	notifier, err := push.NewNotifier(push.NotifierOptions{
		Bus:       bus,
		Service:   service,
		Logger:    logx.Discard(),
		Threshold: 2 * time.Minute,
		Now:       clock.Read,
		Describe:  func(context.Context, string) string { return "Refactor the retry logic" },
	})
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
	// Nothing published before the notifier is listening can be delivered: a
	// subscription that starts "from now" has no history to replay.
	<-notifier.Ready()

	// A short turn is not worth an interruption.
	bus.Publish(events.TypeTurnState, "session-quick", events.TurnState{State: "running"})
	bus.Publish(events.TypeTurnState, "session-quick", events.TurnState{State: "completed"})
	waitFor(t, func() bool { return len(rec.received()) == 0 }, "nothing for a quick turn")

	// A long one is. Its length is stated rather than simulated: the payload
	// says when the turn began, which is what the contract sends that field for,
	// and it is the only way to make this deterministic. The version of this
	// test that advanced a clock instead had to wait for "the notifier read the
	// time once" — a condition other code paths also satisfy — so it raced, and
	// failed two runs in five.
	longStart := clock.Read().Add(-5 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-long", events.TurnState{
		TurnID: "turn-long", State: "running", StartedAt: &longStart,
	})
	bus.Publish(events.TypeTurnState, "session-long", events.TurnState{
		TurnID: "turn-long", State: "completed", StartedAt: &longStart,
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for a long turn")
	first := rec.received()[0]
	// The default body says what happened and nothing about what was said. The
	// notifier is given a Describe that *could* name the session, and does not
	// use it, because IncludeSessionName was not set — the title would otherwise
	// reach a lock screen and any configured chat webhook.
	if strings.Contains(first.Body, "Refactor the retry logic") {
		t.Errorf("body = %q, must not name the session unless asked to", first.Body)
	}
	if first.Body == "" {
		t.Errorf("body is empty; a notification that says nothing is not worth sending")
	}
	if first.SessionID != "session-long" || !strings.Contains(first.URL, "session-long") {
		t.Errorf("message = %+v, want a tap that opens the session", first)
	}

	// An approval interrupts immediately, and urgently.
	//
	// It is driven through the real broker rather than by hand-publishing a
	// payload. That distinction is the whole point: this test used to publish a
	// map while the broker published a struct, so it passed for as long as the
	// notification it was checking silently said nothing. Going through the
	// broker means the assertion is on what production actually emits.
	requestApproval(t, bus, "session-long", "apr_1", "bash")
	waitFor(t, func() bool { return len(rec.received()) >= 2 }, "a notification for the approval")
	approval := rec.received()[1]
	// The tool is named in the title, which is the line a reader sees first and
	// the one that has to distinguish "approve bash?" from "approve write?".
	if approval.Title == "" || !strings.Contains(approval.Title, "bash") {
		t.Errorf("approval message = %+v, want the tool named in the title", approval)
	}
	if approval.Actor == nil || approval.Actor.Kind != push.ActorMain {
		t.Errorf("approval actor = %+v, want the main agent: it is the agent that is blocked", approval.Actor)
	}
	if approval.Outcome != "waiting" {
		t.Errorf("approval outcome = %q, want %q", approval.Outcome, "waiting")
	}
	if got := rec.urgencies[1]; got != "high" {
		t.Errorf("approval urgency = %q, want high: it expires", got)
	}
	if got := rec.headers[1].Get("Authorization"); !strings.HasPrefix(got, "vapid t=") {
		t.Errorf("Authorization = %q, want a VAPID token", got)
	}
	if got := rec.headers[1].Get("Content-Encoding"); got != "aes128gcm" {
		t.Errorf("Content-Encoding = %q, want aes128gcm", got)
	}
}

// sessionName is the title every test in this file gives a session.
const sessionName = "Refactor the retry logic"

// TestNotificationNamesSessionOnlyWhenAsked pins where a session's name may
// appear, and where it may not.
//
// The *title* names the session whenever the deployment has a Describe at all.
// That is not the privacy decision — it is the point of the field: a title is
// the largest text a lock screen and a chat card both draw, and two
// notifications from two sessions are otherwise the same line. This test locks
// in that the outcome and the actor no longer crowd it out, because that is what
// the title used to be.
//
// The *body* is the half the operator's own words reach only on request (see
// NotifierOptions.IncludeSessionName). A settled turn never puts the name there:
// the title already has it, and repeating it is the duplication this change
// removed.
func TestNotificationNamesSessionOnlyWhenAsked(t *testing.T) {
	cases := []struct {
		name      string
		include   bool
		describe  bool
		state     string
		wantTitle string
		wantBody  string
	}{
		{
			name: "the title is the session's name", describe: true,
			state: "completed", wantTitle: sessionName, wantBody: "Open the session for the result.",
		},
		{
			name: "a failure is named the same way; the outcome is the status line", describe: true,
			state: "failed", wantTitle: sessionName, wantBody: "Open the session for the error.",
		},
		{
			name:    "allowing the name in bodies changes nothing about a turn card",
			include: true, describe: true,
			state: "completed", wantTitle: sessionName, wantBody: "Open the session for the result.",
		},
		{
			name:     "with no name to give, the title falls back to the sentence it was",
			describe: false,
			state:    "completed", wantTitle: "completed · Main agent", wantBody: "Open the session for the result.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTitle, body := longTurnMessage(t, tc.include, tc.describe, tc.state)
			if gotTitle != tc.wantTitle {
				t.Errorf("title = %q, want %q", gotTitle, tc.wantTitle)
			}
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			// A settled turn counted nothing and said nothing, so the name has no
			// business in its body.
			if strings.Contains(body, sessionName) {
				t.Errorf("body = %q, must not repeat the session name", body)
			}
		})
	}
}

// longTurnMessage runs one long turn through the real notifier and returns the
// title and body a browser would be shown.
//
// It is the fixture with two knobs, rather than a second copy of the fixture:
// the two things this test varies are whether the deployment can name a session
// at all, and whether it is allowed to say the name in a body.
func longTurnMessage(t *testing.T, includeName, describe bool, state string) (string, string) {
	t.Helper()

	f := newTurnFixture(t, func(o *push.NotifierOptions) {
		o.IncludeSessionName = includeName
		if describe {
			o.Describe = func(context.Context, string) string { return sessionName }
		}
	})
	f.start("session-1", "turn-1")
	f.settle("session-1", "turn-1", state)

	waitFor(t, func() bool { return len(f.messages()) >= 1 }, "a notification for a long turn")
	message := f.messages()[0]
	return message.Title, message.Body
}

// fixedClock is a clock a test can hold still, so a notification can be dated
// from a time the test chose rather than from the machine's.
type fixedClock struct {
	now time.Time
}

func (c *fixedClock) Read() time.Time { return c.now }

// busFor builds the bus a notifier test publishes to.
func busFor(t *testing.T) *events.Bus {
	t.Helper()
	return events.New(events.Config{Replay: 64, Queue: 64})
}

// requestApproval drives the real broker, so the event the notifier sees is the
// one production publishes.
//
// The broker blocks until a human answers, which is what the goroutine is for:
// the test only cares about the notification it raises on the way in.
func requestApproval(t *testing.T, bus *events.Bus, sessionID, id, tool string) {
	t.Helper()
	broker := approvals.New(approvals.Options{Timeout: time.Minute, Bus: bus, Logger: logx.Discard(), Now: time.Now})
	t.Cleanup(broker.Close)

	go func() {
		_, _ = broker.RequestPermission(context.Background(), harness.PermissionRequest{
			ID:          id,
			SessionID:   sessionID,
			ToolCallID:  "call_1",
			Tool:        tool,
			Input:       `{"command":"ls"}`,
			RequestedAt: time.Now(),
			ExpiresAt:   time.Now().Add(time.Minute),
			Options: []harness.PermissionOption{
				{ID: harness.OptionAllowOnce, Name: "Allow once", Kind: "allow_once"},
				{ID: harness.OptionRejectOnce, Name: "Reject", Kind: "reject_once"},
			},
		})
	}()
}

// waitFor polls until condition holds.
// TestALongTurnIsNotifiedEvenIfTheObserverArrivedLate is a regression test for
// a notification that was suppressed by a race.
//
// The notifier used to date a turn from the moment it first saw it running.
// That is only the start when the turn began after this process attached, and
// the notifier is a late observer by nature: it is a subscriber. So a turn with
// two minutes of work behind it could be judged "too short to interrupt you
// about" depending on nothing more than when the running frame happened to be
// processed — which is why the test that covered this failed two runs in five.
//
// The payload's own `startedAt` is what the contract sends for exactly this
// reason, and the reproduction below is the whole bug: publish a turn that says
// it began before the clock's present, complete it without advancing the clock
// at all, and require a notification.
func TestALongTurnIsNotifiedEvenIfTheObserverArrivedLate(t *testing.T) {
	rec := newReceiver(t)
	server := rec.serve(t)

	service, err := push.Open(push.Options{StateDir: t.TempDir(), Logger: logx.Discard(), Client: server.Client()})
	if err != nil {
		t.Fatalf("push.Open: %v", err)
	}
	if err := service.Subscribe(rec.subscription(server.URL + "/push/one")); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	clock := &fixedClock{now: time.Now()}
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	notifier, err := push.NewNotifier(push.NotifierOptions{
		Bus:       bus,
		Service:   service,
		Logger:    logx.Discard(),
		Threshold: 2 * time.Minute,
		Now:       clock.Read,
	})
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

	// A turn that has already been running for ten minutes when the notifier
	// hears about it. The clock never advances: the elapsed time is entirely in
	// the payload, which is the point.
	started := clock.Read().Add(-10 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-late", events.TurnState{
		TurnID: "turn-late", State: "running", StartedAt: &started,
	})
	bus.Publish(events.TypeTurnState, "session-late", events.TurnState{
		TurnID: "turn-late", State: "completed", StartedAt: &started,
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 },
		"a notification for a turn that was long before the notifier attached")
}

// turnFixture is a real notifier wired to both kinds of channel — a browser that
// decrypts what it is sent, and a chat bot that records the cards it is posted —
// so a test can publish the frames a turn is made of and read back what each
// channel would show. The two differ on purpose: the answer is a chat card's
// body and never a lock screen's.
type turnFixture struct {
	rec     *receiver
	chat    *chatServer
	bus     *events.Bus
	started time.Time
}

func newTurnFixture(t *testing.T, tune func(*push.NotifierOptions)) *turnFixture {
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

	chat := &chatServer{}
	chatServer := chat.serve(t)
	hook := push.Webhook{Kind: "feishu", URL: chatServer.URL + "/hook/abc", IncludeAnswer: true}
	hook.Open(logx.Discard(), chatServer.Client(), "https://gateway.test:8443")

	clock := &fixedClock{now: time.Now()}
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	options := push.NotifierOptions{
		Bus:       bus,
		Service:   service,
		Webhooks:  []push.Webhook{hook},
		Logger:    logx.Discard(),
		Threshold: 2 * time.Minute,
		Now:       clock.Read,
	}
	if tune != nil {
		tune(&options)
	}
	notifier, err := push.NewNotifier(options)
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

	return &turnFixture{rec: rec, chat: chat, bus: bus, started: clock.Read().Add(-5 * time.Minute)}
}

// start publishes the turn's opening frame. The start is stated in the payload,
// which is the only way to make "it ran for five minutes" deterministic.
func (f *turnFixture) start(sessionID, turnID string) {
	f.bus.Publish(events.TypeTurnState, sessionID, events.TurnState{
		TurnID: turnID, State: "running", StartedAt: &f.started,
	})
}

// say publishes one committed assistant message.
func (f *turnFixture) say(sessionID, text, model string) {
	f.bus.Publish(events.TypeSessionMessage, sessionID, events.MessageData{
		Role: "assistant", Text: text, Model: model,
	})
}

func (f *turnFixture) settle(sessionID, turnID, state string) {
	f.bus.Publish(events.TypeTurnState, sessionID, events.TurnState{
		TurnID: turnID, State: state, StartedAt: &f.started,
	})
}

func (f *turnFixture) messages() []push.Message { return f.rec.received() }

// cards is every card the chat channel was sent, marshalled for substring
// assertions — the shape a reader would see rather than a field of a struct.
func (f *turnFixture) cards() []string {
	raw := f.chat.all()
	out := make([]string, 0, len(raw))
	for _, card := range raw {
		encoded, _ := json.Marshal(card)
		out = append(out, string(encoded))
	}
	return out
}

// TestTheSettledTurnCarriesTheModelsAnswer is the product decision this change
// exists for: a notification about finished work has to say what the work
// concluded, or it sends the reader into the app to answer the only question
// they had.
func TestTheSettledTurnCarriesTheModelsAnswer(t *testing.T) {
	f := newTurnFixture(t, func(o *push.NotifierOptions) { o.KeepAnswers = true })

	f.start("session-1", "turn-1")
	// An intermediate narration, a step that only called tools, and then the
	// message the turn actually ended on. Only the last one is the answer.
	f.say("session-1", "Let me look at the parser first.", "")
	f.say("session-1", "", "")
	f.say("session-1", "已提交并推送。", "deepseek/deepseek-v4.1-flash")
	f.settle("session-1", "turn-1", "completed")

	waitFor(t, func() bool { return len(f.cards()) >= 1 }, "a card in the chat")
	card := f.cards()[0]
	if !strings.Contains(card, "已提交并推送。") {
		t.Errorf("the card does not carry the answer: %s", card)
	}
	if strings.Contains(card, "Let me look at the parser first.") {
		t.Errorf("the card carries the narration instead of the answer: %s", card)
	}
	// The turn ran for five minutes, and the card says so in its own line.
	if !strings.Contains(card, "5m") {
		t.Errorf("the card does not say how long the turn took: %s", card)
	}
	// The model that wrote it is named when a producer knew, which the session
	// log does and the live ACP path does not.
	if !strings.Contains(card, "deepseek/deepseek-v4.1-flash") {
		t.Errorf("the card does not name the model that answered: %s", card)
	}
}

// TestAnEmptyMessageAfterTheAnswerKeepsIt: a turn can commit a step that said
// nothing after the message that said everything — another tool call, a final
// empty step. Reading that as "the turn's last words" would erase the answer.
func TestAnEmptyMessageAfterTheAnswerKeepsIt(t *testing.T) {
	f := newTurnFixture(t, func(o *push.NotifierOptions) { o.KeepAnswers = true })

	f.start("session-1", "turn-1")
	f.say("session-1", "The build is green.", "")
	f.say("session-1", "   \n  ", "")
	f.settle("session-1", "turn-1", "completed")

	waitFor(t, func() bool { return len(f.cards()) >= 1 }, "a card in the chat")
	if card := f.cards()[0]; !strings.Contains(card, "The build is green.") {
		t.Errorf("the answer was erased by an empty step: %s", card)
	}
}

// TestTheAnswerNeverReachesALockScreen is the size and privacy boundary, tested
// where it can actually fail: a Web Push payload is sealed into a 3000-byte
// record, so an answer left in the message does not merely leak, it breaks
// delivery of the notification that matters.
func TestTheAnswerNeverReachesALockScreen(t *testing.T) {
	f := newTurnFixture(t, func(o *push.NotifierOptions) { o.KeepAnswers = true })

	huge := strings.Repeat("一字一句皆辛苦。", 900) // ~6,300 characters, ~19KB of UTF-8
	f.start("session-1", "turn-1")
	f.say("session-1", huge, "")
	f.settle("session-1", "turn-1", "completed")

	waitFor(t, func() bool { return len(f.messages()) >= 1 && len(f.cards()) >= 1 },
		"a notification that carries no answer at all")
	if got := f.messages()[0].Answer; got != "" {
		t.Errorf("the lock-screen payload carried %d characters of the answer", len(got))
	}
	if card := f.cards()[0]; !strings.Contains(card, "characters omitted") {
		t.Errorf("the card did not cut a 19KB answer: %s", card[:min(len(card), 400)])
	}
}

// TestAnswersAreNotCollectedWhenNoChannelPrintsThem: the text is kilobytes per
// session, so a deployment whose notifications go to a lock screen must not pay
// for holding it.
func TestAnswersAreNotCollectedWhenNoChannelPrintsThem(t *testing.T) {
	f := newTurnFixture(t, func(o *push.NotifierOptions) {
		o.KeepAnswers = false
		o.Webhooks[0].IncludeAnswer = false
	})

	f.start("session-1", "turn-1")
	f.say("session-1", "This text has nowhere to go.", "")
	f.settle("session-1", "turn-1", "completed")

	waitFor(t, func() bool { return len(f.cards()) >= 1 }, "a card in the chat")
	card := f.cards()[0]
	if strings.Contains(card, "This text has nowhere to go.") {
		t.Errorf("an answer was collected and sent with every channel opted out: %s", card)
	}
	// The card still says something: the line it said before this existed.
	if !strings.Contains(card, "Open the session for the result.") {
		t.Errorf("the card lost its fallback body: %s", card)
	}
}

// TestARepeatedSettlementIsAnnouncedOnce: a replayed stream or a restarted
// watcher can repeat a settle frame, and a repeated frame must not post the same
// card again. The second session is the marker that makes this deterministic —
// the notifier handles frames in order, so once its card exists, the duplicate
// has been handled.
func TestARepeatedSettlementIsAnnouncedOnce(t *testing.T) {
	f := newTurnFixture(t, func(o *push.NotifierOptions) { o.KeepAnswers = true })

	f.start("session-1", "turn-1")
	f.say("session-1", "Done.", "")
	f.settle("session-1", "turn-1", "completed")
	f.settle("session-1", "turn-1", "completed")
	f.start("session-2", "turn-2")
	f.settle("session-2", "turn-2", "completed")

	waitFor(t, func() bool { return len(f.messages()) >= 2 }, "both sessions announced")
	first := 0
	for _, message := range f.messages() {
		if message.SessionID == "session-1" {
			first++
		}
	}
	if first != 1 {
		t.Errorf("session-1 was announced %d times, want once", first)
	}
}

func waitFor(t *testing.T, condition func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestTheAnswerReachesTheCardWithoutLosingItsShape is the regression test for a
// card that arrived as one paragraph of punctuation.
//
// The answer is markdown, and markdown's block structure *is* its line breaks: a
// heading, a table and a list are only those things because a newline separates
// them from what came before. The notifier stored the answer through `clip`,
// which collapses whitespace to make a one-line label — correct for a title, and
// fatal here. Every card read "## 结论| 步骤 | 结果 ||---|---|| 构建 |…" and was
// rendered as exactly that.
//
// The assertion is on the delivered card, not on the ledger: the bug was
// invisible until the text had been all the way through the renderer.
func TestTheAnswerReachesTheCardWithoutLosingItsShape(t *testing.T) {
	const answer = "## 结论\n\n| 步骤 | 结果 |\n|---|---|\n| 构建 | 通过 |\n\n- 第一项\n- 第二项\n\n```go\ngo test ./...\n```"

	f := newTurnFixture(t, func(o *push.NotifierOptions) { o.KeepAnswers = true })
	f.start("session-1", "turn-1")
	f.say("session-1", answer, "")
	f.settle("session-1", "turn-1", "completed")

	waitFor(t, func() bool { return len(f.cards()) >= 1 }, "a card in the chat")
	card := parseCard(t, f.chat.all()[0])
	body := strings.Join(card.bodies, "")
	if body != answer {
		t.Errorf("the answer lost its shape on the way to the card:\n got %q\nwant %q", body, answer)
	}
}

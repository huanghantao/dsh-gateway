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
	clock := &watchedClock{now: time.Now()}
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

// TestNotificationNamesSessionOnlyWhenAsked pins the default that keeps a
// notification from carrying the operator's own words.
//
// The text is the part of this system that is displayed on a lock screen and
// mirrored to third-party chat services, so "where does the session name appear"
// is a privacy property rather than a formatting choice, and it is worth a test
// that fails loudly if a future edit makes the name unconditional again.
//
// The two places are deliberately different. The *title* names the session
// whenever the deployment has a Describe at all, because two notifications from
// two sessions are otherwise the same line of text — that is the confusion this
// vocabulary removes. The *body* carries the operator's words only on request.
func TestNotificationNamesSessionOnlyWhenAsked(t *testing.T) {
	const title = "Refactor the retry logic"

	cases := []struct {
		name        string
		include     bool
		state       string
		wantBody    string
		wantTitle   string
		bodyAbsent  string
		titleAbsent string
	}{
		{
			name:       "off by default, and a finished turn still says so",
			include:    false,
			state:      "completed",
			wantBody:   "Open the session for the result.",
			wantTitle:  "completed · Main agent · " + title,
			bodyAbsent: title,
		},
		{
			name:       "off, and a failure is still distinguishable",
			include:    false,
			state:      "failed",
			wantBody:   "Open the session for the error.",
			wantTitle:  "failed · Main agent · " + title,
			bodyAbsent: title,
		},
		{
			name:      "opted in, the body carries the session too",
			include:   true,
			state:     "completed",
			wantBody:  title,
			wantTitle: "completed · Main agent · " + title,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, gotTitle := longTurnMessage(t, tc.include, tc.state, title)
			if body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if gotTitle != tc.wantTitle {
				t.Errorf("title = %q, want %q", gotTitle, tc.wantTitle)
			}
			if tc.bodyAbsent != "" && strings.Contains(body, tc.bodyAbsent) {
				t.Errorf("body = %q, must not contain %q", body, tc.bodyAbsent)
			}
			if tc.titleAbsent != "" && strings.Contains(gotTitle, tc.titleAbsent) {
				t.Errorf("title = %q, must not contain %q", gotTitle, tc.titleAbsent)
			}
		})
	}
}

// longTurnMessage runs one long turn through the real notifier and returns the
// title and body it delivered.
func longTurnMessage(t *testing.T, includeName bool, state, title string) (string, string) {
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

	clock := &watchedClock{now: time.Now()}
	bus := events.New(events.Config{Replay: 64, Queue: 64})
	notifier, err := push.NewNotifier(push.NotifierOptions{
		Bus:                bus,
		Service:            service,
		Logger:             logx.Discard(),
		Threshold:          2 * time.Minute,
		Now:                clock.Read,
		Describe:           func(context.Context, string) string { return title },
		IncludeSessionName: includeName,
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

	// As above: the turn's length is stated in the payload rather than simulated
	// with a clock, so there is nothing to wait for and nothing to race.
	longStart := clock.Read().Add(-5 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{
		TurnID: "turn-1", State: "running", StartedAt: &longStart,
	})
	bus.Publish(events.TypeTurnState, "session-1", events.TurnState{
		TurnID: "turn-1", State: state, StartedAt: &longStart,
	})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for a long turn")
	message := rec.received()[0]
	return message.Body, message.Title
}

// watchedClock is a clock that counts its reads, so a test can wait for the
// notifier to have looked before it changes the time.
type watchedClock struct {
	mu    sync.Mutex
	now   time.Time
	reads int
}

func (c *watchedClock) Read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	return c.now
}

func (c *watchedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *watchedClock) Reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

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

	clock := &watchedClock{now: time.Now()}
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

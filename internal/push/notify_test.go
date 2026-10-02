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

	"github.com/huanghantao/dsh-gateway/internal/app/events"
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
	bus.Publish(events.TypeTurnState, "session-quick", map[string]any{"state": "running"})
	bus.Publish(events.TypeTurnState, "session-quick", map[string]any{"state": "completed"})
	waitFor(t, func() bool { return len(rec.received()) == 0 }, "nothing for a quick turn")

	// A long one is.
	reads := clock.Reads()
	bus.Publish(events.TypeTurnState, "session-long", map[string]any{"state": "running"})
	waitFor(t, func() bool { return clock.Reads() > reads }, "the notifier to note the turn's start")
	clock.Advance(5 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-long", map[string]any{"state": "completed"})

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
	bus.Publish(events.TypeApprovalRequested, "session-long", map[string]any{"id": "apr_1", "tool": "bash"})
	waitFor(t, func() bool { return len(rec.received()) >= 2 }, "a notification for the approval")
	approval := rec.received()[1]
	if approval.Title == "" || !strings.Contains(approval.Body, "bash") {
		t.Errorf("approval message = %+v, want the tool named", approval)
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
// The body is the one part of this system that is displayed on a lock screen and
// mirrored to third-party chat services, so "does it name the session" is a
// privacy property rather than a formatting choice, and it is worth a test that
// fails loudly if a future edit makes the name unconditional again.
func TestNotificationNamesSessionOnlyWhenAsked(t *testing.T) {
	const title = "Refactor the retry logic"

	cases := []struct {
		name    string
		include bool
		state   string
		want    string
		absent  string
	}{
		{
			name:    "off by default, and a finished turn still says so",
			include: false,
			state:   "completed",
			want:    "Open the session for the result.",
			absent:  title,
		},
		{
			name:    "off, and a failure is still distinguishable",
			include: false,
			state:   "failed",
			want:    "The turn failed.",
			absent:  title,
		},
		{
			name:    "opted in, the title is used verbatim",
			include: true,
			state:   "completed",
			want:    title,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := longTurnBody(t, tc.include, tc.state, title)
			if body != tc.want {
				t.Errorf("body = %q, want %q", body, tc.want)
			}
			if tc.absent != "" && strings.Contains(body, tc.absent) {
				t.Errorf("body = %q, must not contain %q", body, tc.absent)
			}
		})
	}
}

// longTurnBody runs one long turn through the real notifier and returns the body
// it delivered.
func longTurnBody(t *testing.T, includeName bool, state, title string) string {
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

	reads := clock.Reads()
	bus.Publish(events.TypeTurnState, "session-1", map[string]any{"state": "running"})
	waitFor(t, func() bool { return clock.Reads() > reads }, "the notifier to note the turn's start")
	clock.Advance(5 * time.Minute)
	bus.Publish(events.TypeTurnState, "session-1", map[string]any{"state": state})

	waitFor(t, func() bool { return len(rec.received()) >= 1 }, "a notification for a long turn")
	return rec.received()[0].Body
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

// eventsApproval is the event type an approval arrives as.
func eventsApproval() events.Type { return events.TypeApprovalRequested }

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

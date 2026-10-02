package v1

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// pushServiceFor gives a test server a push service whose push endpoint is a
// local server, so nothing leaves the machine.
func pushServiceFor(t *testing.T, ts *testServer) (*push.Service, *pushSink, string) {
	t.Helper()
	sink := &pushSink{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.record(r)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	service, err := push.Open(push.Options{
		StateDir: t.TempDir(),
		Logger:   logx.Discard(),
		Client:   server.Client(),
	})
	if err != nil {
		t.Fatalf("push.Open: %v", err)
	}
	ts.deps.Push = service
	ts.deps.Config.Push = config.Push{Enabled: true, Subject: "mailto:test@localhost"}
	// The endpoint a subscription carries is where messages are POSTed, so the
	// test's sink has to *be* the push service rather than merely observe one.
	return service, sink, server.URL
}

// pushSink counts what a push service was asked to deliver.
type pushSink struct {
	mu     sync.Mutex
	bodies int
	auth   []string
}

func (s *pushSink) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bodies++
	s.auth = append(s.auth, r.Header.Get("Authorization"))
}

func (s *pushSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies
}

// subscriptionFor builds a syntactically valid subscription: the API validates
// the keys before storing them, so a placeholder would be rejected.
func subscriptionFor(t *testing.T, endpoint string) string {
	t.Helper()
	// A real P-256 point and a 16-byte secret, base64url without padding.
	const key = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	auth := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	body, err := json.Marshal(map[string]string{"endpoint": endpoint, "p256dh": key, "auth": auth})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(body)
}

// TestPushSubscriptionLifecycle is the settings screen's whole conversation with
// the gateway: what key do I use, here is my subscription, does it work, forget
// it.
func TestPushSubscriptionLifecycle(t *testing.T) {
	ts := newTestServer(t)
	service, sink, pushURL := pushServiceFor(t, ts)

	key := ts.do(http.MethodGet, "/api/v1/push/key", withCookie(ts.token))
	if key.Code != http.StatusOK {
		t.Fatalf("GET /push/key returned %d", key.Code)
	}
	var keyBody struct {
		Enabled   bool   `json:"enabled"`
		PublicKey string `json:"publicKey"`
	}
	if err := json.Unmarshal(key.Body.Bytes(), &keyBody); err != nil {
		t.Fatalf("key is not JSON: %v", err)
	}
	if !keyBody.Enabled || keyBody.PublicKey == "" {
		t.Fatalf("key = %+v, want an enabled gateway with a public key", keyBody)
	}

	endpoint := pushURL + "/push/one"
	rec := ts.do(http.MethodPost, "/api/v1/push/subscribe", withCookie(ts.token), withJSON(subscriptionFor(t, endpoint)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe returned %d, want 201: %s", rec.Code, rec.Body.String())
	}
	subs := service.Subscriptions()
	if len(subs) != 1 || subs[0].Endpoint != endpoint {
		t.Fatalf("service holds %+v, want the subscription just sent", subs)
	}
	if subs[0].DeviceID == "" {
		t.Error("the subscription was stored without the device that sent it")
	}

	// A subscription that cannot be encrypted to is refused rather than stored.
	rec = ts.do(http.MethodPost, "/api/v1/push/subscribe",
		withCookie(ts.token), withJSON(`{"endpoint":"https://push.example.net/two","p256dh":"AAAA","auth":"AAAA"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("subscribing with unusable keys returned %d, want 400", rec.Code)
	}

	// The test button is what makes "is this configured right" answerable.
	rec = ts.do(http.MethodPost, "/api/v1/push/test", withCookie(ts.token))
	if rec.Code != http.StatusOK {
		t.Fatalf("test notification returned %d: %s", rec.Code, rec.Body.String())
	}
	if sink.count() != 1 {
		t.Errorf("the push service received %d request(s), want 1", sink.count())
	}

	rec = ts.do(http.MethodPost, "/api/v1/push/unsubscribe",
		withCookie(ts.token), withJSON(`{"endpoint":"`+endpoint+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("unsubscribe returned %d", rec.Code)
	}
	if len(service.Subscriptions()) != 0 {
		t.Error("the subscription survived an unsubscribe")
	}

	// With nothing registered, a test notification says so instead of pretending.
	rec = ts.do(http.MethodPost, "/api/v1/push/test", withCookie(ts.token))
	if rec.Code != http.StatusConflict {
		t.Errorf("test with no subscription returned %d, want 409", rec.Code)
	}
}

// TestRevokingADeviceForgetsItsNotifications: a phone that is no longer allowed
// to answer an approval must not be told about it.
func TestRevokingADeviceForgetsItsNotifications(t *testing.T) {
	ts := newTestServer(t)
	service, _, pushURL := pushServiceFor(t, ts)

	me := ts.do(http.MethodGet, "/api/v1/me", withCookie(ts.token))
	var principal struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &principal); err != nil {
		t.Fatalf("/me is not JSON: %v", err)
	}

	rec := ts.do(http.MethodPost, "/api/v1/push/subscribe",
		withCookie(ts.token), withJSON(subscriptionFor(t, pushURL+"/phone")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe returned %d", rec.Code)
	}

	rec = ts.do(http.MethodDelete, "/api/v1/devices/"+principal.ID, withCookie(ts.token))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("revoke returned %d: %s", rec.Code, rec.Body.String())
	}
	if subs := service.Subscriptions(); len(subs) != 0 {
		t.Errorf("a revoked device still has %d subscription(s)", len(subs))
	}
}

// TestPushCanBeTurnedOff: the switch is the operator's, and with it off the
// gateway says so rather than half-working.
func TestPushCanBeTurnedOff(t *testing.T) {
	ts := newTestServer(t)
	ts.deps.Config.Push = config.Push{Enabled: false}

	key := ts.do(http.MethodGet, "/api/v1/push/key", withCookie(ts.token))
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(key.Body.Bytes(), &body); err != nil {
		t.Fatalf("key is not JSON: %v", err)
	}
	if body.Enabled {
		t.Error("a disabled gateway advertised push as enabled")
	}

	rec := ts.do(http.MethodPost, "/api/v1/push/test", withCookie(ts.token))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a test notification with push off returned %d, want 503", rec.Code)
	}
}

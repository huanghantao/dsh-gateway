package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// subscriptionsFile holds every registered browser.
const subscriptionsFile = "push.json"

// ErrGone reports that a push service says the subscription no longer exists.
//
// It is a normal end of life — the browser was uninstalled, the permission was
// withdrawn — and the only correct response is to forget the subscription rather
// than to keep sending into a void.
var ErrGone = errors.New("push: subscription is gone")

// Service owns the subscriptions and sends to them.
type Service struct {
	vapid  *VAPID
	client *http.Client
	logger *logx.Logger
	now    func() time.Time

	path string
	mu   sync.Mutex
	subs []Subscription
}

// Options configures a Service.
type Options struct {
	StateDir string
	Subject  string
	Logger   *logx.Logger
	// Client is injectable so a test can point at its own server.
	Client *http.Client
	Now    func() time.Time
}

// Open builds the service, loading whatever subscriptions exist.
func Open(opts Options) (*Service, error) {
	vapid, err := OpenVAPID(opts.StateDir, opts.Subject)
	if err != nil {
		return nil, err
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Service{
		vapid:  vapid,
		client: opts.Client,
		logger: opts.Logger,
		now:    opts.Now,
		path:   filepath.Join(opts.StateDir, subscriptionsFile),
	}

	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s.subs); err != nil {
			// A subscription list is convenience, not state that must be
			// perfect: dropping it costs every phone one re-subscribe.
			s.debug("ignoring an unreadable subscription list", err)
			s.subs = nil
		}
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("push: read subscriptions: %w", err)
	}
	return s, nil
}

// PublicKey is the VAPID key a browser needs in order to subscribe.
func (s *Service) PublicKey() string { return s.vapid.PublicKey() }

// Subscriptions lists what is registered.
func (s *Service) Subscriptions() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Subscription(nil), s.subs...)
}

// Subscribe records a browser, replacing any earlier subscription for the same
// endpoint (a browser re-subscribes when its keys rotate).
func (s *Service) Subscribe(sub Subscription) error {
	if err := sub.Validate(); err != nil {
		return err
	}
	if sub.CreatedAt == "" {
		sub.CreatedAt = s.now().UTC().Format(time.RFC3339)
	}

	s.mu.Lock()
	next := make([]Subscription, 0, len(s.subs)+1)
	for _, existing := range s.subs {
		if existing.Endpoint != sub.Endpoint {
			next = append(next, existing)
		}
	}
	next = append(next, sub)
	s.subs = next
	s.mu.Unlock()

	return s.save()
}

// Unsubscribe forgets one endpoint. Reporting whether anything was removed lets
// the API answer honestly instead of pretending.
func (s *Service) Unsubscribe(endpoint string) (bool, error) {
	s.mu.Lock()
	next := make([]Subscription, 0, len(s.subs))
	removed := false
	for _, existing := range s.subs {
		if existing.Endpoint == endpoint {
			removed = true
			continue
		}
		next = append(next, existing)
	}
	s.subs = next
	s.mu.Unlock()

	if !removed {
		return false, nil
	}
	return true, s.save()
}

// ForgetDevice drops every subscription that belongs to a device, so revoking a
// phone stops its notifications at the same moment it loses its credential.
func (s *Service) ForgetDevice(deviceID string) (int, error) {
	if deviceID == "" {
		return 0, nil
	}
	s.mu.Lock()
	next := make([]Subscription, 0, len(s.subs))
	removed := 0
	for _, existing := range s.subs {
		if existing.DeviceID == deviceID {
			removed++
			continue
		}
		next = append(next, existing)
	}
	s.subs = next
	s.mu.Unlock()

	if removed == 0 {
		return 0, nil
	}
	return removed, s.save()
}

// Send delivers one message to one subscription.
//
// What is encrypted is the lock-screen shape of the message, not the whole of
// it: the answer a chat channel renders has no room in a 3000-byte record and no
// business on a lock screen. See Message.forLockScreen.
func (s *Service) Send(ctx context.Context, sub Subscription, message Message, urgency string) error {
	payload, err := json.Marshal(message.forLockScreen())
	if err != nil {
		return fmt.Errorf("push: encode message: %w", err)
	}
	body, err := Seal(payload, sub)
	if err != nil {
		return err
	}
	auth, err := s.vapid.Authorization(sub.Endpoint)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: request: %w", err)
	}
	request.Header.Set("Content-Encoding", "aes128gcm")
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Authorization", auth)
	// A day, so a notification that arrives while the phone is off is still
	// there when it comes back; the browser drops what it considers stale.
	request.Header.Set("TTL", "86400")
	if urgency != "" {
		request.Header.Set("Urgency", urgency)
	}

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("push: send: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return nil
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		// The browser is gone. Forget it here rather than at every call site.
		if _, err := s.Unsubscribe(sub.Endpoint); err != nil {
			s.debug("could not forget a dead subscription", err)
		}
		return ErrGone
	default:
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("push: %s answered %d: %s", host(sub.Endpoint), response.StatusCode, bytes.TrimSpace(detail))
	}
}

// Broadcast sends to every subscription and reports how many were delivered.
//
// One dead subscription must not stop the others, and one failure must not stop
// the loop either: a phone that is off is not an error.
func (s *Service) Broadcast(ctx context.Context, message Message, urgency string) (sent int, errs []error) {
	for _, sub := range s.Subscriptions() {
		if err := s.Send(ctx, sub, message, urgency); err != nil {
			if errors.Is(err, ErrGone) {
				continue
			}
			errs = append(errs, err)
			continue
		}
		sent++
	}
	return sent, errs
}

// save writes the subscription list atomically.
func (s *Service) save() error {
	s.mu.Lock()
	raw, err := json.MarshalIndent(s.subs, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("push: encode subscriptions: %w", err)
	}
	if err := atomicfile.WriteFileSecret(s.path, raw); err != nil {
		return fmt.Errorf("push: write subscriptions: %w", err)
	}
	return nil
}

func (s *Service) debug(msg string, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Debug("push: "+msg, "error", err.Error())
}

func host(endpoint string) string {
	if parsed, err := url.Parse(endpoint); err == nil {
		return parsed.Host
	}
	return "push service"
}

package v1

import (
	"net/http"
	"strings"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// handlePushKey hands out what a browser needs in order to subscribe.
//
// The public VAPID key is not a secret — it is the identity a push service
// checks signatures against — and the browser cannot subscribe without it, so
// this is one of the few endpoints whose answer is meant to be shared.
func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if s.deps.Push == nil || !s.deps.Config.Push.Enabled {
		_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"publicKey": s.deps.Push.PublicKey(),
		// Which chat channels are configured, named by service rather than by
		// address: the settings screen should be able to say "also sending to
		// feishu" without the webhook — a capability — travelling to a client.
		"channels": channelNames(s.deps.Webhooks),
		// How long a turn has to run before its end is worth a notification, so
		// the settings screen can say what it is promising.
		"turnThresholdSeconds": int(s.deps.Config.Push.TurnThreshold.Std().Seconds()),
		// The address this gateway is *meant* to be reached at, and the
		// certificate a phone has to trust before any of this works. A client
		// that finds itself on the wrong origin, or on a page whose certificate
		// the browser rejects, cannot fix either by itself — and a service
		// worker will not install on such a page at all, which is the failure
		// that looks most like nothing happening.
		"appURL": s.deps.Config.PublicURL,
		"caURL":  certificateURL(s.deps.Config.PublicURL),
	})
}

// handlePushSubscribe records this device's subscription.
func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.Push == nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "push_disabled", "this gateway does not send notifications"))
		return
	}
	var sub push.Subscription
	if err := httpcore.DecodeJSON(w, r, &sub); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	// The device is the caller's, never the body's: a phone must not be able to
	// register itself as another phone, or revoking one would not stop it.
	principal, _ := principalFrom(r.Context())
	sub.DeviceID = principal.DeviceID

	if err := s.deps.Push.Subscribe(sub); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.Wrap(err, errx.KindInvalid, "invalid_subscription", "that subscription cannot be used"))
		return
	}
	s.deps.Audit.Record(r.Context(), "push.subscribed", principal.DeviceID, map[string]any{
		"endpointHost": hostOf(sub.Endpoint),
	})
	_ = httpcore.RespondJSON(w, http.StatusCreated, map[string]any{"subscribed": true})
}

// handlePushUnsubscribe forgets one endpoint.
func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	if s.deps.Push == nil {
		_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"removed": false})
		return
	}
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := httpcore.DecodeJSON(w, r, &req); err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	removed, err := s.deps.Push.Unsubscribe(req.Endpoint)
	if err != nil {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.Wrap(err, errx.KindInternal, "push_failed", "the subscription could not be removed"))
		return
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// handlePushTest sends one notification to this device, now.
//
// It exists because "did I configure this right" is otherwise unanswerable until
// something important happens and does not arrive.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Push == nil || !s.deps.Config.Push.Enabled {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnavailable, "push_disabled", "this gateway does not send notifications"))
		return
	}
	principal, _ := principalFrom(r.Context())

	message := push.Message{
		Title: "dsh-gateway",
		Body:  "Notifications are working.",
		URL:   "./#/sessions",
		Tag:   "test",
	}

	sent := 0
	var last error
	for _, sub := range s.deps.Push.Subscriptions() {
		if sub.DeviceID != principal.DeviceID {
			continue
		}
		if err := s.deps.Push.Send(r.Context(), sub, message, "high"); err != nil {
			last = err
			continue
		}
		sent++
	}

	// The chat channels are the ones that work on a phone whose network cannot
	// reach Google's push service, so a test that only tried Web Push would say
	// "not subscribed" to an operator whose notifications do work.
	channels := 0
	for i := range s.deps.Webhooks {
		hook := &s.deps.Webhooks[i]
		if err := hook.Send(r.Context(), message); err != nil {
			last = err
			continue
		}
		channels++
	}

	if sent == 0 && channels == 0 {
		text := "this device has no push subscription and no chat channel is configured"
		if last != nil {
			text = last.Error()
		}
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindConflict, "push_not_subscribed", text))
		return
	}
	_ = httpcore.RespondJSON(w, http.StatusOK, map[string]any{"sent": sent, "channels": channels})
}

// channelNames lists the configured chat channels by service.
func channelNames(webhooks []push.Webhook) []string {
	names := make([]string, 0, len(webhooks))
	for _, hook := range webhooks {
		names = append(names, hook.Kind)
	}
	return names
}

// certificateURL is where this deployment publishes its CA root, or "" when it
// does not run one (a publicly trusted certificate needs no download).
func certificateURL(publicURL string) string {
	if !strings.HasPrefix(publicURL, "https://") {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + "/ca.crt"
}

// hostOf names a push service for the audit log without recording the endpoint,
// which is a capability: anyone holding it can send to that browser.
func hostOf(endpoint string) string {
	for i := 0; i < len(endpoint); i++ {
		if endpoint[i] == '/' && i > 8 {
			return endpoint[:i]
		}
	}
	return "unknown"
}

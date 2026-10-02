package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Webhook is a chat channel that receives the same notifications Web Push does.
//
// Why it exists: Web Push on Android is Google's push service and nothing else —
// a phone whose network cannot reach it, or which has no Google Play services at
// all, can never receive one. A group bot has its own delivery path, works on any
// phone, and puts the notification somewhere the operator is already looking.
type Webhook struct {
	// Kind selects the message format. Today only "feishu" is implemented; the
	// field exists so that a second provider is a case in this file rather than
	// a change to the configuration's shape.
	Kind string `yaml:"kind" json:"kind"`
	// URL is the bot's incoming-webhook address. It is a capability: anyone
	// holding it can post to that chat, so it lives with the other secrets and is
	// never echoed back to a client.
	URL string `yaml:"url" json:"-"`
	// BaseURL is the public address of the gateway, used to turn a notification's
	// relative link into one a chat client can open.
	BaseURL string `yaml:"-" json:"-"`

	logger *logx.Logger
	client *http.Client
}

// KnownWebhookKinds are the providers this build can post to.
var KnownWebhookKinds = []string{"feishu"}

// Validate checks a webhook before it is used.
func (w Webhook) Validate() error {
	switch w.Kind {
	case "feishu":
	default:
		return fmt.Errorf("push: unknown webhook kind %q (known: %s)", w.Kind, strings.Join(KnownWebhookKinds, ", "))
	}
	parsed, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("push: webhook url: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("push: webhook url must be https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("push: webhook url has no host")
	}
	return nil
}

// Host names the webhook's service for logs and status, without the secret path.
func (w Webhook) Host() string {
	if parsed, err := url.Parse(w.URL); err == nil {
		return parsed.Host
	}
	return "webhook"
}

// Open attaches the runtime pieces a webhook needs.
func (w *Webhook) Open(logger *logx.Logger, client *http.Client, baseURL string) {
	w.logger = logger
	w.BaseURL = strings.TrimRight(baseURL, "/")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	w.client = client
}

// Send posts one notification.
func (w *Webhook) Send(ctx context.Context, message Message) error {
	if err := w.Validate(); err != nil {
		return err
	}
	payload, err := w.payload(message)
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("push: webhook request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := w.client.Do(request)
	if err != nil {
		return fmt.Errorf("push: webhook: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("push: %s answered %d: %s", w.Host(), response.StatusCode, bytes.TrimSpace(body))
	}
	// A chat webhook answers 200 with a body that says whether it accepted the
	// message, and refusing silently is exactly the failure a test button is
	// meant to catch — "Key Words Not Found" arrives this way.
	var reply struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &reply); err == nil && reply.Code != 0 {
		return fmt.Errorf("push: %s refused the card: %s (code %d)", w.Host(), reply.Msg, reply.Code)
	}
	return nil
}

// payload renders the message in the provider's format.
func (w *Webhook) payload(message Message) ([]byte, error) {
	switch w.Kind {
	case "feishu":
		return w.feishuPayload(message)
	default:
		return nil, fmt.Errorf("push: unknown webhook kind %q", w.Kind)
	}
}

// feishuPayload builds an interactive card.
//
// A card rather than plain text because of the button: the point of the
// notification is to get the operator to the session, and a card can carry the
// link as a control instead of a URL they have to select out of a paragraph.
func (w *Webhook) feishuPayload(message Message) ([]byte, error) {
	colour := "blue"
	if message.Tag != "" && strings.HasPrefix(message.Tag, "approval-") {
		// An approval expires; the card says so in the only way a chat client
		// lets us: colour.
		colour = "orange"
	}

	body := message.Body
	if body == "" {
		body = message.Title
	}
	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": message.Title},
			"template": colour,
		},
		"elements": []any{
			map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": body}},
		},
	}

	if link := w.absolute(message.URL); link != "" {
		// Comma-ok rather than a bare assertion: errcheck is right that an
		// assertion can panic, and the checked form costs nothing here.
		elements, _ := card["elements"].([]any)
		card["elements"] = append(elements, map[string]any{
			"tag": "action",
			"actions": []any{map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": "Open the session"},
				"type": "primary",
				"url":  link,
			}},
		})
	}

	return json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
}

// absolute turns a notification's app-relative link into one a chat client can
// open. A notification that says "open the session" and then cannot is worse
// than one that says nothing.
func (w *Webhook) absolute(relative string) string {
	if relative == "" || w.BaseURL == "" {
		return ""
	}
	if strings.HasPrefix(relative, "http://") || strings.HasPrefix(relative, "https://") {
		return relative
	}
	// The app is mounted at /m/, and its router lives in the fragment.
	return w.BaseURL + "/m/" + strings.TrimPrefix(relative, "./")
}

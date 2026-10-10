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
	"strings"
	"sync/atomic"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Webhook is a chat channel that receives the same notifications Web Push does.
//
// Why it exists: Web Push on Android is Google's push service and nothing else —
// a phone whose network cannot reach it, or which has no Google Play services at
// all, can never receive one. A group bot has its own delivery path, works on any
// phone, and puts the notification somewhere the operator is already looking.
//
// This type is the *channel*: an address, a kind, and the HTTP that posts to it.
// What a given service is shown lives beside it — feishu.go for the card, and
// answer.go for how much of a model's answer any chat message may carry — so
// that adding a provider is a new file plus a case in payload, not a new branch
// in the transport.
type Webhook struct {
	// Kind selects the message format. Today only "feishu" is implemented; the
	// field exists so that a second provider is a case in payload rather than a
	// change to the configuration's shape.
	Kind string `yaml:"kind" json:"kind"`
	// URL is the bot's incoming-webhook address. It is a capability: anyone
	// holding it can post to that chat, so it lives with the other secrets and is
	// never echoed back to a client.
	URL string `yaml:"url" json:"-"`
	// BaseURL is the public address of the gateway, used to turn a notification's
	// relative link into one a chat client can open.
	BaseURL string `yaml:"-" json:"-"`
	// IncludeAnswer decides whether the card carries the model's own closing
	// message instead of a one-line count of the work.
	//
	// It is a per-channel choice rather than a property of the notification,
	// because the same turn is reported to a lock screen and to a chat group, and
	// only one of those is a place a deployment may be willing to send model
	// output: a group has other people in it, a chat service stores what it is
	// sent, and the answer is the first text in a notification that the operator
	// did not write and cannot predict. See NotifierOptions.CarryAnswers, which
	// decides whether any notification is allowed to carry the text at all — a
	// group cannot print what the deployment never let travel.
	IncludeAnswer bool `yaml:"includeAnswer" json:"includeAnswer"`
	// MaxAnswerChars bounds how much of that answer one card prints. Zero takes
	// the built-in budget, DefaultAnswerChars.
	MaxAnswerChars int `yaml:"maxAnswerChars" json:"maxAnswerChars"`

	logger *logx.Logger
	client *http.Client
	// state remembers what this channel's service accepts. It is a pointer so
	// that copying a Webhook — which the configuration layer does, and which a
	// struct holding a lock would make `go vet` refuse — shares one answer rather
	// than forking it. Nil when Open was never called, which only happens in a
	// test that is not sending anything.
	state *channelState
}

// channelState is what a channel learns about its service while running.
type channelState struct {
	// plain is set once the service has refused a rich card, so the wedged
	// format is not offered again on every notification.
	plain atomic.Bool
}

// refusal is a chat service answering "no" to a card it understood.
//
// It is told apart from a transport failure on purpose: a timeout says nothing
// about whether the card was well formed, and downgrading a channel because the
// network blinked would silently cost the deployment its rich cards. Only an
// answer from the service counts as a verdict on the format.
type refusal struct {
	host string
	code int
	msg  string
}

func (r *refusal) Error() string {
	return fmt.Sprintf("push: %s refused the card: %s (code %d)", r.host, r.msg, r.code)
}

// KnownWebhookKinds are the providers this build can post to.
var KnownWebhookKinds = []string{"feishu"}

// cardFormat is which dialect of a provider's card to build.
//
// It exists because "does this chat service accept the newest card shape?" is not
// a question this gateway can answer offline, and a wrong guess is not a
// cosmetic failure: a refused card is a notification nobody gets. So the rich
// format is attempted, a *refusal* downgrades this channel to the plain one, and
// the answer is remembered — see Send.
type cardFormat int

const (
	// cardRich is the current format: Feishu card JSON 2.0, whose markdown
	// component renders what a model actually writes.
	cardRich cardFormat = iota
	// cardPlain is the first-generation format, which lark_md limits to bold,
	// links and mentions. Its body is folded to fit; see flattenMarkdown.
	cardPlain
)

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
	w.state = &channelState{}
}

// Send posts one notification.
//
// The richest format the service accepts is the one that gets posted, and that is
// decided by asking rather than by configuration: a rich card refused by the
// service is retried as a plain one, and the channel remembers which it is for
// every notification after that. The alternative — a `cardFormat` key the
// operator has to get right — would be asking them to know a fact only the
// service does, and getting it wrong means no notification at all.
func (w *Webhook) Send(ctx context.Context, message Message) error {
	if err := w.Validate(); err != nil {
		return err
	}

	if !w.isPlain() {
		err := w.post(ctx, message, cardRich)
		if err == nil {
			return nil
		}
		var denied *refusal
		if !errors.As(err, &denied) {
			// The service never answered for the card, so it has said nothing
			// about whether it understands it.
			return err
		}
		w.usePlain(denied)
	}
	return w.post(ctx, message, cardPlain)
}

// isPlain reports whether this channel has been downgraded.
func (w *Webhook) isPlain() bool {
	return w.state != nil && w.state.plain.Load()
}

// usePlain records a refusal once, with the reason, so that the one line an
// operator reads when their cards look plain explains why.
func (w *Webhook) usePlain(denied *refusal) {
	if w.state != nil {
		w.state.plain.Store(true)
	}
	if w.logger != nil {
		w.logger.Warn("push: this chat service refused a card in the current format; using the plain one from now on",
			"kind", w.Kind, "host", denied.host, "code", denied.code, "reason", denied.msg)
	}
}

// post builds one card and delivers it.
func (w *Webhook) post(ctx context.Context, message Message, format cardFormat) error {
	payload, err := w.payload(message, format)
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
	if response.StatusCode == http.StatusBadRequest {
		// A 400 is the service saying it could not read the request, which for
		// this caller means one thing: the card. It is a verdict on the format,
		// so it downgrades — and it is the shape a card 2.0 is refused in by
		// endpoints that do not answer with a JSON code. Verified against the
		// live service: a 2.0 card sent with the wrong envelope comes back
		// exactly this way, "parse card json err".
		return &refusal{host: w.Host(), code: response.StatusCode, msg: strings.TrimSpace(string(body))}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Everything else — a 401, a 429, a 500, a timeout — is not a statement
		// about the card, and downgrading on one would cost the deployment its
		// rich cards for a reason nobody could see.
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
		return &refusal{host: w.Host(), code: reply.Code, msg: reply.Msg}
	}
	return nil
}

// payload renders the message in the provider's format.
//
// This switch is the whole extension point: a new chat service is two builders in
// its own file named here, plus its name in KnownWebhookKinds and Validate. It is
// deliberately the only place that knows a provider's name and its renderers at
// once.
func (w *Webhook) payload(message Message, format cardFormat) ([]byte, error) {
	switch w.Kind {
	case "feishu":
		if format == cardPlain {
			return w.feishuPlainPayload(message)
		}
		return w.feishuPayload(message)
	default:
		return nil, fmt.Errorf("push: unknown webhook kind %q", w.Kind)
	}
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

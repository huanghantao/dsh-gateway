package push_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// chatServer stands in for a group bot: it records the cards it is sent.
type chatServer struct {
	mu    sync.Mutex
	cards []map[string]any
	reply string
	http  int
	// denyRich makes the bot behave like a service that never learned card 2.0:
	// anything carrying a schema is refused, everything else is accepted. It is
	// the one behaviour a gateway cannot know offline, and therefore the one the
	// fallback exists for.
	denyRich bool
	// richHTTP expresses that refusal as an HTTP status instead of a JSON code,
	// which is how one of Feishu's own endpoints answers a card it cannot parse.
	richHTTP int
}

func (c *chatServer) serve(t *testing.T) *httptest.Server {
	t.Helper()
	// TLS, because a webhook is only ever https: the URL carries the capability
	// to post into someone's chat, and validation refuses to pretend otherwise.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("the card is not JSON: %v", err)
		}
		c.mu.Lock()
		c.cards = append(c.cards, payload)
		reply, status, denyRich, richHTTP := c.reply, c.http, c.denyRich, c.richHTTP
		c.mu.Unlock()

		if isRichCard(payload) {
			if denyRich {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"code":19001,"msg":"card schema not supported"}`))
				return
			}
			if richHTTP != 0 {
				w.WriteHeader(richHTTP)
				_, _ = w.Write([]byte(`{"code":200621,"msg":"parse card json err"}`))
				return
			}
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if reply == "" {
			reply = `{"code":0,"msg":"success"}`
		}
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	return server
}

// isRichCard reports whether a payload carries a card 2.0 schema.
func isRichCard(payload map[string]any) bool {
	card, ok := payload["card"].(map[string]any)
	if !ok {
		return false
	}
	schema, _ := card["schema"].(string)
	return schema != ""
}

func (c *chatServer) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cards) == 0 {
		return nil
	}
	return c.cards[len(c.cards)-1]
}

// all returns every card the channel was sent, in order.
func (c *chatServer) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.cards...)
}

// TestFeishuCardCarriesTheSessionAndALink is the whole point of the channel: a
// notification that cannot take the operator to the session is a notification
// they have to act on twice.
func TestFeishuCardCarriesTheSessionAndALink(t *testing.T) {
	chat := &chatServer{}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc"}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	err := hook.Send(context.Background(), push.Message{
		Title:     "Approval needed",
		Body:      "Approve bash? · 把发布说明整理成文档",
		URL:       "./#/sessions/session-42",
		Tag:       "approval-apr_1",
		SessionID: "session-42",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	card := chat.last()
	if card == nil {
		t.Fatal("no card was posted")
	}
	if card["msg_type"] != "interactive" {
		t.Errorf("msg_type = %v, want an interactive card", card["msg_type"])
	}
	encoded, _ := json.Marshal(card)
	body := string(encoded)
	for _, want := range []string{
		"Approval needed",
		"把发布说明整理成文档",
		"https://gateway.test:8443/m/#/sessions/session-42",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card does not mention %q: %s", want, body)
		}
	}
	// An approval is the one that expires; the colour is the only urgency a chat
	// client gives us.
	if !strings.Contains(body, "orange") {
		t.Errorf("an approval card is not marked as urgent: %s", body)
	}
}

// TestFeishuRefusalIsAnError: the bot answers 200 with a code, and a mistaken
// keyword rule is exactly the kind of refusal that would otherwise look like a
// delivered notification.
func TestFeishuRefusalIsAnError(t *testing.T) {
	chat := &chatServer{reply: `{"code":19024,"msg":"Key Words Not Found"}`}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc"}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test")

	err := hook.Send(context.Background(), push.Message{Title: "hi"})
	if err == nil {
		t.Fatal("a refused card was reported as delivered")
	}
	if !strings.Contains(err.Error(), "Key Words Not Found") {
		t.Errorf("error = %v, want the bot's own message", err)
	}
}

// TestWebhookValidation keeps a misconfigured channel from failing at the worst
// possible moment — when there is something to say.
func TestWebhookValidation(t *testing.T) {
	tests := []struct {
		name string
		hook push.Webhook
	}{
		{"unknown kind", push.Webhook{Kind: "slack", URL: "https://x.test/hook"}},
		{"plain http", push.Webhook{Kind: "feishu", URL: "http://x.test/hook"}},
		{"no host", push.Webhook{Kind: "feishu", URL: "https://"}},
		{"empty", push.Webhook{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.hook.Validate(); err == nil {
				t.Error("Validate accepted a webhook that cannot be used")
			}
		})
	}
	if err := (push.Webhook{Kind: "feishu", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/x"}).Validate(); err != nil {
		t.Errorf("a real webhook was rejected: %v", err)
	}
}

// TestNotifierReachesChatChannels: the notifier must deliver to a chat channel on
// its own, with no browsers subscribed at all — which is the situation this
// channel exists for.
func TestNotifierReachesChatChannels(t *testing.T) {
	chat := &chatServer{}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc"}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	bus := busFor(t)
	notifier, err := push.NewNotifier(push.NotifierOptions{
		Bus:      bus,
		Webhooks: []push.Webhook{hook},
		Logger:   logx.Discard(),
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}
	if names := notifier.Channels(); len(names) != 1 || !strings.HasPrefix(names[0], "feishu@") {
		t.Errorf("channels = %v, want the chat channel named", names)
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

	requestApproval(t, bus, "session-9", "apr_9", "rm -rf build")
	waitFor(t, func() bool { return chat.last() != nil }, "a card in the chat")

	if card := chat.last(); card != nil {
		encoded, _ := json.Marshal(card)
		if !strings.Contains(string(encoded), "rm -rf build") {
			t.Errorf("the card does not name the tool the agent is waiting on: %s", encoded)
		}
	}
}

// cardParts is a rendered card, taken apart by the role each element plays.
//
// It reads both dialects: a card 2.0 keeps its elements under `body` and says its
// text with `markdown`, while a card 1.0 keeps them at the root and has separate
// `note`, `div` and `action` elements. A test should not have to care which
// dialect it is looking at — that is what Send decides — only what the reader
// ends up seeing.
type cardParts struct {
	title    string
	template string
	notes    []string
	// noteSize is the text size the status line asked for, which is how a test
	// pins the one property that makes it a footnote rather than a line of body.
	noteSize string
	bodies   []string
	buttons  []string
	links    []string
	raw      string
}

func parseCard(t *testing.T, payload map[string]any) cardParts {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("card is not JSON: %v", err)
	}
	parts := cardParts{raw: string(encoded)}

	// The recorded payload is the request body — {"msg_type": …, "card": {…}} —
	// which is the shape the bot receives rather than the shape a reader sees.
	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("the payload carries no card: %s", parts.raw)
	}
	header, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatalf("the card has no header: %s", parts.raw)
	}
	title, _ := header["title"].(map[string]any)
	parts.title, _ = title["content"].(string)
	parts.template, _ = header["template"].(string)

	elements, _ := card["elements"].([]any)
	if body, ok := card["body"].(map[string]any); ok {
		elements, _ = body["elements"].([]any)
	}

	// A 2.0 card says its status line and its body with the same element, and the
	// rule between them is what tells the two apart: the layout is always
	// [status?, rule?, body?], so text before the rule is a status line.
	sawRule := false
	for _, element := range elements {
		item, ok := element.(map[string]any)
		if !ok {
			continue
		}
		switch item["tag"] {
		case "hr":
			sawRule = true
		case "markdown":
			content, _ := item["content"].(string)
			size, _ := item["text_size"].(string)
			if sawRule {
				parts.bodies = append(parts.bodies, content)
			} else {
				parts.notes = append(parts.notes, content)
				parts.noteSize = size
			}
		case "note":
			nested, _ := item["elements"].([]any)
			for _, one := range nested {
				content, _ := one.(map[string]any)
				text, _ := content["content"].(string)
				parts.notes = append(parts.notes, text)
			}
		case "div":
			text, _ := item["text"].(map[string]any)
			content, _ := text["content"].(string)
			parts.bodies = append(parts.bodies, content)
		case "button":
			text, _ := item["text"].(map[string]any)
			label, _ := text["content"].(string)
			parts.buttons = append(parts.buttons, label)
			behaviors, _ := item["behaviors"].([]any)
			for _, one := range behaviors {
				behavior, _ := one.(map[string]any)
				url, _ := behavior["default_url"].(string)
				parts.links = append(parts.links, url)
			}
		case "action":
			actions, _ := item["actions"].([]any)
			for _, one := range actions {
				action, _ := one.(map[string]any)
				text, _ := action["text"].(map[string]any)
				label, _ := text["content"].(string)
				parts.buttons = append(parts.buttons, label)
				url, _ := action["url"].(string)
				parts.links = append(parts.links, url)
			}
		}
	}
	return parts
}

// sendCard posts one message to a recording bot and returns the card as rendered.
func sendCard(t *testing.T, hook push.Webhook, message push.Message) cardParts {
	t.Helper()

	chat := &chatServer{}
	server := chat.serve(t)
	hook.URL = server.URL + "/hook/abc"
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	if err := hook.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %v", err)
	}
	card := chat.last()
	if card == nil {
		t.Fatal("no card was posted")
	}
	return parseCard(t, card)
}

// TestTheAnswerIsTheBodyAndTheNameIsTheTitle is the card's whole contract.
//
//	┌──────────────────────────────────────────┐
//	│ <the session's own name>                 │  the largest text, and the one
//	├──────────────────────────────────────────┤  that tells two cards apart
//	│ ✅ Completed · Main agent · 🕐 9m 37s …  │  status, small and grey
//	│ ──────────────────────────────────────── │
//	│ <the model's closing message>            │  the answer, in full
//	│ ──────────────────────────────────────── │
//	│ [ Open the session ]                     │
//	└──────────────────────────────────────────┘
func TestTheAnswerIsTheBodyAndTheNameIsTheTitle(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title:      "现在我们的dsh gateway在子agent完",
		Body:       "5 tool calls",
		Answer:     "已提交并推送。\n\n- 提交：`4b41a31`",
		Summary:    "5 tool calls",
		Outcome:    "completed",
		Actor:      &push.Actor{Kind: push.ActorMain},
		DurationMS: (9*time.Minute + 37*time.Second).Milliseconds(),
		URL:        "./#/sessions/session-42",
		Tag:        "turn-session-42",
		SessionID:  "session-42",
	})

	if parts.title != "现在我们的dsh gateway在子agent完" {
		t.Errorf("title = %q, want the session's name alone", parts.title)
	}
	// Success is a colour, because that is the only thing a chat client draws
	// before the reader reads a word.
	if parts.template != "green" {
		t.Errorf("template = %q, want green for a completed turn", parts.template)
	}
	if len(parts.bodies) != 1 || !strings.Contains(parts.bodies[0], "已提交并推送。") {
		t.Errorf("bodies = %q, want the model's answer", parts.bodies)
	}
	// The answer is not summarised, reworded or trimmed: it is the answer.
	if !strings.Contains(strings.Join(parts.bodies, ""), "- 提交：`4b41a31`") {
		t.Errorf("the answer lost its own formatting: %q", parts.bodies)
	}
	if len(parts.notes) != 1 {
		t.Fatalf("notes = %q, want exactly one status line", parts.notes)
	}
	note := parts.notes[0]
	for _, want := range []string{"✅ Completed", "Main agent", "🕐 9m 37s", "5 tool calls"} {
		if !strings.Contains(note, want) {
			t.Errorf("status line = %q, want it to say %q", note, want)
		}
	}
	if len(parts.buttons) != 1 || parts.buttons[0] != "Open the session" {
		t.Errorf("buttons = %q, want the one action", parts.buttons)
	}
}

// TestALongAnswerKeepsItsHeadAndTail: an answer over the budget is not silently
// cut. A reader who cannot see that something was dropped cannot know to open
// the session, and the counts are exact so that they can tell a paragraph they
// can skip from half a document they cannot.
func TestALongAnswerKeepsItsHeadAndTail(t *testing.T) {
	answer := "# 文字质量评审\n\n开头是结论。\n" +
		strings.Repeat("中间的过程写得很长。", 500) +
		"\n最后一行是结论。"
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true, MaxAnswerChars: 400}, push.Message{
		Title:   "你整体看看我们的项目",
		Answer:  answer,
		Outcome: "completed",
		URL:     "./#/sessions/session-1",
	})

	body := strings.Join(parts.bodies, "")
	if !strings.Contains(body, "开头是结论。") || !strings.Contains(body, "最后一行是结论。") {
		t.Errorf("the cut answer lost its head or its tail: %q", body)
	}
	if !strings.Contains(body, "characters omitted") {
		t.Errorf("the cut answer does not say how much it dropped: %q", body)
	}
	if len(parts.buttons) != 1 || parts.buttons[0] != "Open the session for the rest" {
		t.Errorf("buttons = %q, want the reader pointed at the rest", parts.buttons)
	}
}

// TestAnAnswerThatFitsIsNotTouched: the budget is a ceiling, not a style. The
// answers main agents actually produce are small — half of them under 30
// characters — and a card that reformats those would be a card that rewrites the
// model.
func TestAnAnswerThatFitsIsNotTouched(t *testing.T) {
	const answer = "已提交并推送。\n\n- 提交：`4b41a31`\n- 推送：`33a4829..4b41a31`"
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title: "会话", Answer: answer, Outcome: "completed",
	})
	if got := strings.Join(parts.bodies, ""); got != answer {
		t.Errorf("body = %q, want the answer verbatim", got)
	}
	if strings.Contains(parts.raw, "characters omitted") {
		t.Error("a short answer was reported as cut")
	}
}

// TestAChannelWithoutAnswersBehavesAsItDidBefore: the answer is opt-in per
// channel, and a deployment that turns it off must still get a card — the line
// the product used to send, not a hole where a body should be.
func TestAChannelWithoutAnswersBehavesAsItDidBefore(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu"}, push.Message{
		Title:     "Refactor the retry logic",
		Body:      "12 tool calls · 3 files changed",
		Answer:    "This must not appear.",
		Summary:   "12 tool calls · 3 files changed",
		Outcome:   "completed",
		SessionID: "session-7",
	})
	if got := strings.Join(parts.bodies, ""); got != "12 tool calls · 3 files changed" {
		t.Errorf("body = %q, want the message's own body", got)
	}
	if strings.Contains(parts.raw, "This must not appear") {
		t.Errorf("a channel with answers off printed one: %s", parts.raw)
	}
	if parts.title != "Refactor the retry logic" {
		t.Errorf("title = %q, want the session name whatever the body policy", parts.title)
	}
}

// TestAFailureIsRedAndSaysWhereTheErrorIs.
func TestAFailureIsRedAndSaysWhereTheErrorIs(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title:     "重构 push 包的通知策略",
		Body:      "push: webhook refused the card: Key Words Not Found (code 19024)",
		Summary:   "5 tool calls · 2 failures",
		Outcome:   "failed",
		Actor:     &push.Actor{Kind: push.ActorMain},
		URL:       "./#/sessions/session-3",
		SessionID: "session-3",
	})
	if parts.template != "red" {
		t.Errorf("template = %q, want red for a failure", parts.template)
	}
	if len(parts.buttons) != 1 || parts.buttons[0] != "Open the session for the error" {
		t.Errorf("buttons = %q, want the reader pointed at the error", parts.buttons)
	}
	if !strings.Contains(strings.Join(parts.notes, ""), "❌ Failed") {
		t.Errorf("status line = %q, want the failure named", parts.notes)
	}
}

// TestAModelCannotMentionTheGroup: the body of a card is text the gateway did
// not write — it is whatever the model read and repeated — and one lark_md tag
// makes a bot notify people. A repository containing that string must not be a
// way to ping a five-hundred-person group.
func TestAModelCannotMentionTheGroup(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title:   "审查这个仓库",
		Answer:  "README 里有一行：<at id=all></at> 请忽略它。",
		Outcome: "completed",
	})
	body := strings.Join(parts.bodies, "")
	if strings.Contains(body, "<at") {
		t.Errorf("the card carries a live mention: %q", body)
	}
	if !strings.Contains(body, "id=all") {
		t.Errorf("the text was mangled rather than defused: %q", body)
	}
}

// TestAnAnswerCutInsideACodeBlockIsClosed: a card renders markdown, so an odd
// number of fences turns everything after it into one long code block — a cut
// answer would take the rest of the card with it.
func TestAnAnswerCutInsideACodeBlockIsClosed(t *testing.T) {
	answer := "改完了，这是 diff：\n\n```go\n" + strings.Repeat("+ func handler() {}\n", 200)
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true, MaxAnswerChars: 300}, push.Message{
		Title: "重构 handler", Answer: answer, Outcome: "completed",
	})
	body := strings.Join(parts.bodies, "")
	if strings.Count(body, "```")%2 != 0 {
		t.Errorf("the card leaves a code fence open: %q", body)
	}
}

// TestAnApprovalIsStillAnApproval: approvals are the one notification whose
// subject is an action rather than a conversation, so the tool leads and the
// card is orange — the colour that says it expires.
func TestAnApprovalIsStillAnApproval(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu"}, push.Message{
		Title:     "Approval needed · bash",
		Body:      "The agent is waiting for your decision.",
		Outcome:   "waiting",
		Actor:     &push.Actor{Kind: push.ActorMain},
		Summary:   "bash",
		Tag:       "approval-apr_1",
		SessionID: "session-5",
	})
	if parts.template != "orange" {
		t.Errorf("template = %q, want orange for an approval", parts.template)
	}
	// The tool is in the title, so the status line must not repeat it.
	if note := strings.Join(parts.notes, ""); strings.Contains(note, "bash") {
		t.Errorf("status line = %q, repeats the title", note)
	}
}

// TestTheCardIsNotHuge: the budget exists so that one answer cannot push a
// group's other messages off the screen, and so that the card stays inside what
// a chat client accepts.
func TestTheCardIsNotHuge(t *testing.T) {
	answer := strings.Repeat("这是一句很长的回答。", 5000) // 50,000 characters
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title: "长回答", Answer: answer, Outcome: "completed",
	})
	if size := len(parts.raw); size > 32*1024 {
		t.Errorf("card is %d bytes, want it bounded well inside a chat client's limit", size)
	}
}

// TestARefusedRichCardFallsBackAndStaysThere is the answer to the one question
// this gateway cannot settle offline: does the operator's chat service understand
// card 2.0?
//
// It is settled by asking, once. The rich card is attempted; a service that
// refuses it gets the plain card instead, and the channel remembers, so the
// deployment pays one refused request in total rather than one per notification.
// The refusal is logged with the service's own words, because "my cards look
// plain" is otherwise a mystery.
func TestARefusedRichCardFallsBackAndStaysThere(t *testing.T) {
	chat := &chatServer{denyRich: true}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc", IncludeAnswer: true}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	message := push.Message{
		Title:     "重构 push 包",
		Body:      "5 tool calls",
		Answer:    "## 结论\n\n- 第一条\n- 第二条\n\n```go\ngo test ./...\n```",
		Summary:   "5 tool calls",
		Outcome:   "completed",
		Actor:     &push.Actor{Kind: push.ActorMain},
		URL:       "./#/sessions/session-1",
		SessionID: "session-1",
	}

	if err := hook.Send(context.Background(), message); err != nil {
		t.Fatalf("Send: %v", err)
	}
	cards := chat.all()
	if len(cards) != 2 {
		t.Fatalf("posted %d cards, want the refused rich one and the plain one", len(cards))
	}
	if !isRichCard(cards[0]) {
		t.Error("the first attempt was not a card 2.0; the fallback proves nothing")
	}
	if isRichCard(cards[1]) {
		t.Error("the retry was a card 2.0 as well, so it will be refused too")
	}

	plain := parseCard(t, cards[1])
	body := strings.Join(plain.bodies, "")
	for _, want := range []string{"**结论**", "• 第一条", "go test ./..."} {
		if !strings.Contains(body, want) {
			t.Errorf("the plain card does not say %q: %q", want, body)
		}
	}
	if strings.Contains(body, "```") || strings.Contains(body, "##") {
		t.Errorf("the plain card kept markdown its reader cannot see rendered: %q", body)
	}

	// And it stays downgraded: one more notification, one more card, no retry.
	if err := hook.Send(context.Background(), message); err != nil {
		t.Fatalf("Send after the downgrade: %v", err)
	}
	if got := len(chat.all()); got != 3 {
		t.Errorf("a downgraded channel offered the rich card again: %d cards posted", got)
	}
}

// TestATransportFailureIsNotACardRefusal: a timeout or a 500 says nothing about
// whether the service understands the card, and downgrading on one would cost the
// deployment its rich cards for a reason nobody could see.
func TestATransportFailureIsNotACardRefusal(t *testing.T) {
	chat := &chatServer{http: http.StatusInternalServerError}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc"}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	err := hook.Send(context.Background(), push.Message{Title: "会话", Body: "5 tool calls"})
	if err == nil {
		t.Fatal("a 500 was reported as delivered")
	}
	if got := len(chat.all()); got != 1 {
		t.Errorf("posted %d times after a transport failure, want no retry", got)
	}
}

// TestTheRichCardRendersMarkdown: the point of the 2.0 format is the body an
// agent actually writes — headings, bullets, tables and code — arriving as
// markdown rather than as punctuation.
func TestTheRichCardRendersMarkdown(t *testing.T) {
	const answer = "## 结论\n\n| 文件 | 行数 |\n|---|---|\n| webhook.go | 171 |\n\n```go\ngo test ./...\n```"
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title: "重构 push 包", Answer: answer, Outcome: "completed", URL: "./#/sessions/session-1",
	})

	card, _ := json.Marshal(parts.raw)
	if !strings.Contains(string(card), `\"schema\":\"2.0\"`) {
		t.Fatalf("the card is not a 2.0 card: %s", parts.raw)
	}
	body := strings.Join(parts.bodies, "")
	if body != answer {
		t.Errorf("the rich card rewrote the answer:\n got %q\nwant %q", body, answer)
	}
}

// TestAnHTTP400OnTheRichCardIsAlsoARefusal: endpoints differ in how they say "I
// cannot read this card" — a JSON code, or a bare 400. Both are verdicts on the
// format and both must downgrade, or a deployment whose service answers this way
// loses the notification entirely instead of getting the plain card.
func TestAnHTTP400OnTheRichCardIsAlsoARefusal(t *testing.T) {
	chat := &chatServer{richHTTP: http.StatusBadRequest}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc", IncludeAnswer: true}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	err := hook.Send(context.Background(), push.Message{
		Title: "会话", Body: "5 tool calls", Answer: "## 结论\n\n正文", Outcome: "completed",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	cards := chat.all()
	if len(cards) != 2 {
		t.Fatalf("posted %d cards, want the refused rich one and the plain one", len(cards))
	}
	if !isRichCard(cards[0]) || isRichCard(cards[1]) {
		t.Error("the retry was not the plain card")
	}
}

// TestAServerErrorIsNotARefusal: 500 and 429 are not statements about the card.
// Downgrading on one would take the markdown rendering away for a reason that
// says nothing about the deployment's chat service.
func TestAServerErrorIsNotARefusal(t *testing.T) {
	chat := &chatServer{richHTTP: http.StatusInternalServerError}
	server := chat.serve(t)

	hook := push.Webhook{Kind: "feishu", URL: server.URL + "/hook/abc", IncludeAnswer: true}
	hook.Open(logx.Discard(), server.Client(), "https://gateway.test:8443")

	if err := hook.Send(context.Background(), push.Message{Title: "会话", Body: "5 tool calls"}); err == nil {
		t.Fatal("a 500 was reported as delivered")
	}
	if got := len(chat.all()); got != 1 {
		t.Errorf("posted %d times after a 500, want no retry", got)
	}
}

// TestTheStatusLineIsAFootnote pins the property that makes the card readable
// rather than merely correct.
//
// The status line — "✅ Completed · Main agent · 🕐 9m 37s · 5 tool calls · …" —
// is metadata about the answer, and at body size it competes with the answer for
// the reader's first glance. `text_size: notation` is the one property that
// demotes it, and both alternatives were posted to a live tenant before this
// test was written: a header subtitle takes the header's colour, stays on one
// line, and truncates a status this long; leaving it at body size is what the
// first version of this card did.
func TestTheStatusLineIsAFootnote(t *testing.T) {
	parts := sendCard(t, push.Webhook{Kind: "feishu", IncludeAnswer: true}, push.Message{
		Title:      "会话",
		Answer:     "回答正文。",
		Summary:    "5 tool calls",
		Outcome:    "completed",
		Actor:      &push.Actor{Kind: push.ActorMain},
		DurationMS: (9*time.Minute + 37*time.Second).Milliseconds(),
	})
	if len(parts.notes) != 1 {
		t.Fatalf("notes = %q, want exactly one status line", parts.notes)
	}
	if parts.noteSize != "notation" {
		t.Errorf("status line text_size = %q, want %q", parts.noteSize, "notation")
	}
}

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

	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// chatServer stands in for a group bot: it records the cards it is sent.
type chatServer struct {
	mu    sync.Mutex
	cards []map[string]any
	reply string
	http  int
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
		reply, status := c.reply, c.http
		c.mu.Unlock()

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

func (c *chatServer) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cards) == 0 {
		return nil
	}
	return c.cards[len(c.cards)-1]
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

	bus.Publish(eventsApproval(), "session-9", map[string]any{"id": "apr_9", "tool": "rm -rf build"})
	waitFor(t, func() bool { return chat.last() != nil }, "a card in the chat")

	if card := chat.last(); card != nil {
		encoded, _ := json.Marshal(card)
		if !strings.Contains(string(encoded), "rm -rf build") {
			t.Errorf("the card does not name the tool the agent is waiting on: %s", encoded)
		}
	}
}

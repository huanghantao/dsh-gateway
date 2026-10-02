package push_test

import (
	"context"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/push"
)

// TestSendReachesADecryptingBrowser isolates the transport from the notifier.
func TestSendReachesADecryptingBrowser(t *testing.T) {
	rec := newReceiver(t)
	server := rec.serve(t)
	service, err := push.Open(push.Options{StateDir: t.TempDir(), Logger: logx.Discard(), Client: server.Client()})
	if err != nil {
		t.Fatalf("push.Open: %v", err)
	}
	sub := rec.subscription(server.URL + "/push/one")
	if err := service.Subscribe(sub); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := service.Send(context.Background(), sub, push.Message{Title: "hi", Body: "there"}, "high"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.received(); len(got) != 1 || got[0].Title != "hi" {
		t.Fatalf("received %+v", got)
	}
}

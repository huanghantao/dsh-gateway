package sessionlog

import (
	"context"
	"testing"
)

// TestReceiptSumsUpASession is the whole feature in one assertion: a reader asks
// "what did it do", and the answer comes from the log rather than from anything
// the gateway had to remember.
func TestReceiptSumsUpASession(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	store := newTestStore(t, root)

	receipt, err := store.Receipt(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}

	if receipt.Title != "why is the upload failing?" || receipt.Workspace != "/Users/me/code/api" {
		t.Errorf("receipt = %+v, want the session's own title and workspace", receipt)
	}
	if receipt.Messages != 2 {
		t.Errorf("messages = %d, want 2 (one prompt, one answer)", receipt.Messages)
	}
	if receipt.Turns != 1 {
		t.Errorf("turns = %d, want 1", receipt.Turns)
	}

	// Two bash calls, one of which failed.
	if len(receipt.Tools) != 1 || receipt.Tools[0].Name != "bash" {
		t.Fatalf("tools = %+v, want one bash tally", receipt.Tools)
	}
	if receipt.Tools[0].Calls != 2 || receipt.Tools[0].Failed != 1 {
		t.Errorf("bash tally = %+v, want 2 calls and 1 failure", receipt.Tools[0])
	}

	// Tokens are the sum of what every step processed, which is what a cost
	// conversation is about — not the context size of the last message.
	if receipt.InputTokens != 5195 || receipt.OutputTokens != 42 {
		t.Errorf("tokens = %d in / %d out, want 5195 / 42", receipt.InputTokens, receipt.OutputTokens)
	}
	if receipt.ContextTokens != 10317 {
		t.Errorf("context = %d, want the last message's total", receipt.ContextTokens)
	}
	if receipt.SpanSeconds < 0 || receipt.ActiveSeconds < 0 {
		t.Errorf("durations = %d span / %d active, want non-negative", receipt.SpanSeconds, receipt.ActiveSeconds)
	}
	if receipt.StartedAt.IsZero() || receipt.EndedAt.IsZero() {
		t.Error("the receipt does not say when the session ran")
	}
}

// TestReceiptCountsFilesItTouched: "which files did this change" is the question
// asked most often a day later, and the answer is in the tool arguments.
func TestReceiptCountsFilesItTouched(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, []string{
		`{"type":"session","version":4,"id":"` + testSession + `","cwd":"/tmp/ws"}`,
		`{"type":"user/message","seq":1,"time":1790519693046,"data":{"content":[{"type":"text","text":"do it"}],"id":"u1","source":{"kind":"user"}}}`,
		`{"type":"tool/call","seq":2,"time":1790519693100,"data":{"callId":"c1","name":"read","arguments":"{\"file_path\":\"/tmp/ws/src/a.ts\"}"}}`,
		`{"type":"tool/result","seq":3,"time":1790519693200,"data":{"message":{"toolCallId":"c1","isError":false,"id":"r1","content":[{"type":"text","text":"ok"}]}}}`,
		`{"type":"tool/call","seq":4,"time":1790519693300,"data":{"callId":"c2","name":"edit","arguments":"{\"file_path\":\"/tmp/ws/src/b.ts\",\"old_string\":\"a\",\"new_string\":\"b\"}"}}`,
		`{"type":"tool/result","seq":5,"time":1790519693400,"data":{"message":{"toolCallId":"c2","isError":false,"id":"r2","content":[{"type":"text","text":"ok"}]}}}`,
		// The same file again: a receipt lists it once.
		`{"type":"tool/call","seq":6,"time":1790519693500,"data":{"callId":"c3","name":"read","arguments":"{\"file_path\":\"/tmp/ws/src/a.ts\"}"}}`,
		`{"type":"tool/result","seq":7,"time":1790519693600,"data":{"message":{"toolCallId":"c3","isError":false,"id":"r3","content":[{"type":"text","text":"ok"}]}}}`,
		// A tool whose arguments mean nothing to this build must not break it.
		`{"type":"tool/call","seq":8,"time":1790519693700,"data":{"callId":"c4","name":"mystery","arguments":"not json at all"}}`,
		`{"type":"tool/result","seq":9,"time":1790519693800,"data":{"message":{"toolCallId":"c4","isError":false,"id":"r4","content":[{"type":"text","text":"ok"}]}}}`,
	})
	store := newTestStore(t, root)

	receipt, err := store.Receipt(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	want := []string{"/tmp/ws/src/a.ts", "/tmp/ws/src/b.ts"}
	if len(receipt.Files) != len(want) {
		t.Fatalf("files = %v, want %v", receipt.Files, want)
	}
	for i := range want {
		if receipt.Files[i] != want[i] {
			t.Errorf("files = %v, want %v (in the order they were touched)", receipt.Files, want)
		}
	}
	if len(receipt.Tools) != 3 {
		t.Errorf("tools = %+v, want read, edit and the unknown one tallied", receipt.Tools)
	}
}

// TestReceiptIgnoresIdleTime: a session left open overnight is not eight hours of
// work, and a figure that said so would be worse than no figure.
func TestReceiptIgnoresIdleTime(t *testing.T) {
	root := t.TempDir()
	base := int64(1790519693000)
	writeLog(t, root, "--ws--", testSession, []string{
		`{"type":"session","version":4,"id":"` + testSession + `","cwd":"/tmp/ws"}`,
		`{"type":"user/message","seq":1,"time":` + itoa(base) + `,"data":{"content":[{"type":"text","text":"work"}],"id":"u1","source":{"kind":"user"}}}`,
		// A four-hour gap: the session sat there between the prompt and the
		// answer, which is not work.
		`{"type":"assistant/message","seq":2,"time":` + itoa(base+4*3600*1000) + `,"data":{"message":{"id":"a1","content":[{"type":"text","text":"done"}],"source":{"model":"m"}}}}`,
		// Then two entries two minutes apart, which is work.
		`{"type":"tool/call","seq":3,"time":` + itoa(base+4*3600*1000+120_000) + `,"data":{"callId":"c1","name":"bash","arguments":"{}"}}`,
	})
	store := newTestStore(t, root)

	receipt, err := store.Receipt(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Receipt: %v", err)
	}
	if receipt.ActiveSeconds > 200 {
		t.Errorf("active seconds = %d, want the four-hour gap discarded", receipt.ActiveSeconds)
	}
	if receipt.ActiveSeconds < 120 {
		t.Errorf("active seconds = %d, want the two-minute gap counted", receipt.ActiveSeconds)
	}
	// The span is the session's whole life, idle stretch included: it answers
	// "how long was this open", which is a different question from "how long was
	// the agent working" and both are worth having.
	if receipt.SpanSeconds < 4*3600 {
		t.Errorf("span = %d, want the whole life including the idle stretch", receipt.SpanSeconds)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestReceiptCostsWhatTheSessionUsed is the arithmetic a reader can check: three
// disjoint token counts, each multiplied by its own price.
func TestReceiptCostsWhatTheSessionUsed(t *testing.T) {
	receipt := Receipt{Model: "deepseek/deepseek-v4.1-flash", InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 2_000_000}
	priced := receipt.WithCost([]Price{{
		Model:     "deepseek-v4.1-flash",
		Currency:  "CNY",
		Input:     2,
		Output:    8,
		CacheRead: 0.4,
	}})
	if priced.Cost == nil {
		t.Fatal("a session with a configured price has no cost")
	}
	// 1M fresh at 2 + 0.5M out at 8 + 2M cached at 0.4 = 2 + 4 + 0.8.
	if got := priced.Cost.Total; got < 6.79 || got > 6.81 {
		t.Errorf("total = %v, want 6.8", got)
	}
	if priced.Cost.Currency != "CNY" {
		t.Errorf("currency = %q, want the configured one", priced.Cost.Currency)
	}

	// A model nobody priced gets no number at all: an invented cost is worse
	// than a missing one.
	unknown := receipt.WithCost([]Price{{Model: "some-other-model", Currency: "CNY", Input: 1}})
	if unknown.Cost != nil {
		t.Errorf("cost = %+v, want nothing for an unpriced model", unknown.Cost)
	}
	if none := receipt.WithCost(nil); none.Cost != nil {
		t.Error("a gateway with no price table produced a cost")
	}
	// The shape a config template ships must not read as "this was free".
	placeholder := receipt.WithCost([]Price{{Model: "deepseek/deepseek-v4.1-flash", Currency: "CNY"}})
	if placeholder.Cost != nil {
		t.Errorf("cost = %+v, want nothing until real prices are filled in", placeholder.Cost)
	}
}

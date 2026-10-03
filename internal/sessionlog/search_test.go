package sessionlog

import (
	"context"
	"strings"
	"testing"
)

// TestSearchFindsTextInsideSessions is the point of the feature: a reader
// remembers asking about something, not which session it was in.
func TestSearchFindsTextInsideSessions(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", "session-cache", []string{
		`{"type":"session","version":4,"id":"session-cache","cwd":"/Users/me/code/api"}`,
		`{"type":"session/title","seq":1,"time":1790519693049,"data":{"title":"构建缓存问题"}}`,
		`{"type":"user/message","seq":2,"time":1790519693050,"data":{"content":[{"type":"text","text":"构建缓存一直不生效，怎么办？"}],"id":"u1","source":{"kind":"user"}}}`,
		`{"type":"assistant/message","seq":3,"time":1790519693060,"data":{"message":{"id":"a1","content":[{"type":"text","text":"先清掉本地副本再试一次。"}],"source":{"model":"m"}}}}`,
	})
	writeLog(t, root, "--ws--", "session-other", []string{
		`{"type":"session","version":4,"id":"session-other","cwd":"/Users/me/code/api"}`,
		`{"type":"user/message","seq":1,"time":1790519693050,"data":{"content":[{"type":"text","text":"把日志归档"}],"id":"u1","source":{"kind":"user"}}}`,
	})
	store := newTestStore(t, root)

	// The query is in the *body*, which is what this search adds: the list
	// search already matches titles.
	matches, scanned, err := store.Search(context.Background(), "缓存", DefaultSearchOptions())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want both sessions read", scanned)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want one", matches)
	}
	match := matches[0]
	if match.SessionID != "session-cache" || match.Title != "构建缓存问题" {
		t.Errorf("match = %+v, want the session it is in, named", match)
	}
	if !strings.Contains(match.Snippet, "缓存") {
		t.Errorf("snippet = %q, want the text around the match", match.Snippet)
	}
	if match.Role != string(RoleUser) || match.Seq != 2 {
		t.Errorf("match = %+v, want the user message it came from", match)
	}
}

// TestSearchFindsToolOutputAndNamesTheField covers the question a coding
// agent's history is most often asked: "which session printed that?" The answer
// is only in the output, and the result has to say so — a hit that looked like
// something the operator typed would send them looking in the wrong place.
func TestSearchFindsToolOutputAndNamesTheField(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", "session-build", []string{
		`{"type":"session","version":4,"id":"session-build","cwd":"/Users/me/code/api"}`,
		`{"type":"user/message","seq":1,"time":1790519693050,"data":{"content":[{"type":"text","text":"run the tests"}],"id":"u1","source":{"kind":"user"}}}`,
		`{"type":"tool/call","seq":2,"time":1790519693060,"data":{"callId":"c1","name":"bash","arguments":"{\"command\":\"go test ./...\"}"}}`,
		`{"type":"tool/result","seq":3,"time":1790519693070,"data":{"message":{"toolCallId":"c1","isError":true,"content":[{"type":"text","text":"panic: nil map write in internal/cache/store.go:41"}]}}}`,
	})
	store := newTestStore(t, root)

	matches, _, err := store.Search(context.Background(), "nil map write", DefaultSearchOptions())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %+v, want the session whose tool printed it", matches)
	}
	match := matches[0]
	if match.Field != "output" {
		t.Errorf("field = %q, want output", match.Field)
	}
	if match.Tool != "bash" {
		t.Errorf("tool = %q, want the command that produced it", match.Tool)
	}
	if !strings.Contains(match.Snippet, "nil map write") {
		t.Errorf("snippet = %q, want the text around the match, taken from the output", match.Snippet)
	}
}

// TestSearchPrefersWhatWasSaidOverWhatWasPrinted pins the field order: a phrase
// that appears both in a prompt and in some tool's output is reported as the
// prompt, because that is where a reader expects to find their own words.
func TestSearchPrefersWhatWasSaidOverWhatWasPrinted(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", "session-both", []string{
		`{"type":"session","version":4,"id":"session-both","cwd":"/ws"}`,
		`{"type":"user/message","seq":1,"time":1790519693050,"data":{"content":[{"type":"text","text":"why is the upload failing"}],"id":"u1","source":{"kind":"user"}}}`,
		`{"type":"tool/call","seq":2,"time":1790519693060,"data":{"callId":"c1","name":"bash","arguments":"{\"command\":\"grep upload\"}"}}`,
		`{"type":"tool/result","seq":3,"time":1790519693070,"data":{"message":{"toolCallId":"c1","content":[{"type":"text","text":"upload: connection reset"}]}}}`,
	})
	store := newTestStore(t, root)

	matches, _, err := store.Search(context.Background(), "upload", DefaultSearchOptions())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no match for a phrase that appears twice")
	}
	if matches[0].Field != "text" {
		t.Errorf("field = %q, want text: the operator's own words come first", matches[0].Field)
	}
}

// TestSearchCutsSnippetsOnRunes: this product's text is mostly Chinese, and a
// byte-wise cut would show broken characters that look like the session is
// corrupted.
func TestSearchCutsSnippetsOnRunes(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("中文内容", 40) + "关键词" + strings.Repeat("结尾内容", 40)
	writeLog(t, root, "--ws--", testSession, []string{
		`{"type":"session","version":4,"id":"` + testSession + `","cwd":"/ws"}`,
		`{"type":"user/message","seq":1,"time":1790519693050,"data":{"content":[{"type":"text","text":"` + long + `"}],"id":"u1","source":{"kind":"user"}}}`,
	})
	store := newTestStore(t, root)

	matches, _, err := store.Search(context.Background(), "关键词", DefaultSearchOptions())
	if err != nil || len(matches) != 1 {
		t.Fatalf("matches = %+v err = %v, want one", matches, err)
	}
	snippet := matches[0].Snippet
	if !strings.Contains(snippet, "关键词") {
		t.Errorf("snippet = %q, want the match in it", snippet)
	}
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") {
		t.Errorf("snippet = %q, want ellipses on both sides of a mid-text match", snippet)
	}
	if strings.ContainsRune(snippet, '\ufffd') {
		t.Errorf("snippet contains a replacement character: %q", snippet)
	}
	// A snippet is a window, not the message.
	if len([]rune(snippet)) > snippetWidth*2 {
		t.Errorf("snippet is %d runes, want a window", len([]rune(snippet)))
	}
}

// TestSearchIsBoundedAndReportsWhatItRead: searching cannot be answered from a
// cache, so it is capped — and the cap is visible, not silent.
func TestSearchIsBoundedAndReportsWhatItRead(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"session-a", "session-b", "session-c"} {
		writeLog(t, root, "--ws--", id, []string{
			`{"type":"session","version":4,"id":"` + id + `","cwd":"/ws"}`,
			`{"type":"user/message","seq":1,"time":1790519693050,"data":{"content":[{"type":"text","text":"needle here"}],"id":"u1","source":{"kind":"user"}}}`,
		})
	}
	store := newTestStore(t, root)

	matches, scanned, err := store.Search(context.Background(), "needle", SearchOptions{Sessions: 2, PerSession: 1, Matches: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if scanned != 2 {
		t.Errorf("scanned = %d, want the bound respected", scanned)
	}
	if len(matches) != 2 {
		t.Errorf("matches = %d, want one per scanned session", len(matches))
	}

	if matches, _, err := store.Search(context.Background(), "   ", DefaultSearchOptions()); err != nil || matches != nil {
		t.Errorf("an empty query searched anyway: %+v %v", matches, err)
	}
}

package push

// Tests for the pieces a chat message is assembled from, as functions.
//
// webhook_test.go proves the card a reader ends up with; these pin the edge cases
// of the arithmetic underneath it, which is where a cut goes wrong: an empty
// string, a budget of exactly the answer's length, a fence opened and never
// closed. They are in the package rather than outside it because the functions
// are the implementation — a test that had to build a card to reach them would be
// testing the card.

import (
	"strings"
	"testing"
	"time"
)

func TestAnswerTextKeepsWhatFits(t *testing.T) {
	cases := []struct {
		name          string
		answer        string
		limit         int
		wantUntouched bool
	}{
		{"an answer well inside the budget", "已提交并推送。", 100, true},
		{"exactly the budget", strings.Repeat("字", 100), 100, true},
		{"one over the budget", strings.Repeat("字", 101), 100, false},
		{"a budget of zero means no budget", strings.Repeat("字", 5000), 0, true},
		{"a budget of minus one is not a budget", "短", -1, true},
		{"empty", "", 100, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := answerText(tc.answer, tc.limit)
			if truncated == tc.wantUntouched {
				t.Errorf("truncated = %v for %q at limit %d", truncated, tc.name, tc.limit)
			}
			if tc.wantUntouched && got != tc.answer {
				t.Errorf("an answer inside the budget was rewritten:\n got %q\nwant %q", got, tc.answer)
			}
		})
	}
}

// TestAnswerTextStatesTheCut: the count is the whole point of the marker. A
// reader who is told "some of it was dropped" still cannot tell a paragraph they
// can skip from half a document they cannot.
func TestAnswerTextStatesTheCut(t *testing.T) {
	answer := strings.Repeat("字", 1000)
	got, truncated := answerText(answer, 100)
	if !truncated {
		t.Fatal("a 1000-character answer fitted into a 100-character budget")
	}
	if !strings.Contains(got, "900 characters omitted, of 1000") {
		t.Errorf("the marker does not account for the cut: %q", got)
	}
	// The head comes first and the tail last, in that order.
	if !strings.HasPrefix(got, "字") || !strings.HasSuffix(got, "字") {
		t.Errorf("head or tail is missing: %q", got)
	}
}

// TestAnswerTextClosesAnOpenFence is the failure that would take the rest of a
// card with it: markdown renders everything after an odd fence as code.
func TestAnswerTextClosesAnOpenFence(t *testing.T) {
	answer := "这是 diff：\n\n```go\n" + strings.Repeat("+ line\n", 200)
	got, truncated := answerText(answer, 100)
	if !truncated {
		t.Fatal("the answer was not cut, so nothing was tested")
	}
	if strings.Count(got, "```")%2 != 0 {
		t.Errorf("the cut left a fence open: %q", got)
	}
}

func TestCutAtLine(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		// The case that used to panic: an empty head is not a line to cut back to.
		{"empty", "", ""},
		{"no newline", "abcdefghij", "abcdefghij"},
		{"a break at the very end", "abcdefghi\n", "abcdefghi"},
		{"a break too far back to be worth it", "\nabcdefghij", "\nabcdefghij"},
		{"a break inside the last tenth", strings.Repeat("a", 90) + "\nbcdefghij", strings.Repeat("a", 90)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutAtLine(tc.text); got != tc.want {
				t.Errorf("cutAtLine(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestCutAtLineFrom(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"empty", "", ""},
		{"a break at the very start", "\nabcdefghij", "abcdefghij"},
		{"no newline", "abcdefghij", "abcdefghij"},
		{"a break too late to be worth it", "abcdefghij\nklm", "abcdefghij\nklm"},
		{"a break inside the first tenth", "ab\n" + strings.Repeat("c", 90), strings.Repeat("c", 90)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutAtLineFrom(tc.text); got != tc.want {
				t.Errorf("cutAtLineFrom(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestBalanceFences(t *testing.T) {
	cases := []struct{ name, text string }{
		{"no fence", "prose only"},
		{"a matched pair", "```go\nx\n```"},
		{"two matched pairs", "```a``` and ```b```"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := balanceFences(tc.text); got != tc.text {
				t.Errorf("balanceFences rewrote balanced text: %q", got)
			}
		})
	}
	if got := balanceFences("```go\nx"); !strings.HasSuffix(got, "```") {
		t.Errorf("balanceFences left an open fence open: %q", got)
	}
}

// TestHumanDuration pins the units, because "0s" is a claim and a gap is not.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Second, ""},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m"},
		{9*time.Minute + 37*time.Second, "9m 37s"},
		{time.Hour, "1h"},
		{2*time.Hour + 5*time.Minute, "2h 5m"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOutcomeStatus pins the vocabulary a reader scans: the same words the wire
// uses, with the mark that makes them scannable, and nothing at all for an
// outcome this build does not know.
func TestOutcomeStatus(t *testing.T) {
	cases := map[string]string{
		"completed": "✅ Completed",
		"failed":    "❌ Failed",
		"cancelled": "⏹ Stopped",
		"waiting":   "⏳ Waiting for you",
		"expired":   "⌛ Expired",
		"":          "",
		"invented":  "",
	}
	for outcome, want := range cases {
		if got := outcomeStatus(outcome); got != want {
			t.Errorf("outcomeStatus(%q) = %q, want %q", outcome, got, want)
		}
	}
}

// TestTheAnswerBudgetAndClipAreNotInterchangeable pins the difference the two
// functions exist to express: one makes a label, the other preserves a document.
//
// It used to be keepShape against clip, and it is answerText against clip now:
// keepShape was the notifier's own copy of the rule, applied to a model's answer
// on its way into the ledger it kept. There is no ledger — the answer arrives on
// the settlement — so the rule is applied where the text is actually printed,
// which is this card's own budget.
func TestTheAnswerBudgetAndClipAreNotInterchangeable(t *testing.T) {
	const document = "## 结论\n\n- 一\n- 二"

	if got, cut := answerText(document, 100); got != document || cut {
		t.Errorf("answerText rewrote a document it had room for:\n got %q (cut=%v)\nwant %q", got, cut, document)
	}
	if got := clip(document, 100); strings.Contains(got, "\n") {
		t.Errorf("clip kept a line break, so it is no longer a label-maker: %q", got)
	}
	// The label-maker still bounds by runes, and still does not cut a multi-byte
	// character in half.
	long := strings.Repeat("字", 50)
	if got, want := clip(long, 10), strings.Repeat("字", 10)+"…"; got != want {
		t.Errorf("clip(%d runes, limit 10) = %q, want %q", 50, got, want)
	}
}

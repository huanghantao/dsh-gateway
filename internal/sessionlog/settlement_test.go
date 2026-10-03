package sessionlog

import (
	"context"
	"testing"
)

// TestProjectSubagentSettlement covers the record that answers "which agent
// finished, and how did it go?" — and that used to be dropped on the floor.
//
// DSH delivers a delegated child's settlement by splicing it into the session's
// inbox, with a typed source: `kind: "subagent-settled"`, `form: "notice"`, its
// own one-line summary, and the child's session id. The projection skipped every
// splice as bookkeeping, so a phone watching a desk session was never told a
// child had settled at all — it saw the main agent's turns and nothing else.
//
// The fixture below is the shape a real log carries, trimmed to the fields the
// projection reads. The event is a `agent/inbox/spliced` with three content
// blocks, because that is how the harness writes it: the sentence, the marker,
// and the report, which is why the joined text runs "…send it more.Its closing
// message:" without a separating newline.
func TestProjectSubagentSettlement(t *testing.T) {
	const child = "d19ffb0f-60c3-49a2-83f0-591341325c02"
	root := t.TempDir()
	events := []string{
		`{"type":"session","version":4,"id":"session-11111111-2222-4333-8444-555555555555","createdAt":1790519692979,"cwd":"/Users/me/code/api"}`,
		`{"type":"user/message","seq":8,"time":1790519693046,"data":{"content":[{"type":"text","text":"review the branch"}],"id":"m-user-1","source":{"kind":"user"}}}`,
		// A relay: the child's report delivered to the model. It is the same text
		// the settlement repeats as its closing message, so projecting both would
		// show one child's answer twice.
		`{"type":"agent/inbox/spliced","seq":445,"time":1791011983470,"data":{"target":"next-step","start":0,"inserted":[{"content":[{"type":"text","text":"Agent ` + child + ` sent a message: "},{"type":"text","text":"RESEARCH RESULT — the report."}],"source":{"kind":"agent-message","form":"relay","senderSessionId":"` + child + `"},"role":"user","id":"relay-1"}]}}`,
		// The settlement itself.
		`{"type":"agent/inbox/spliced","seq":446,"time":1791011992753,"data":{"target":"next-step","start":1,"inserted":[{"content":[{"type":"text","text":"Background subagent ` + child + ` finished and will do no further work unless you send it more."},{"type":"text","text":"Its closing message:"},{"type":"text","text":"Report delivered to the parent agent."}],"source":{"kind":"subagent-settled","form":"notice","summary":"Background subagent ` + child + ` finished and will do no further work unless you send it more.","senderSessionId":"` + child + `"},"role":"user","id":"a452de0c-c5e4-4d8e-b6e8-8043b25ad2c0"}]}}`,
		`{"type":"turn/end","seq":447,"time":1791011993000,"data":{"turn":1,"reason":{"kind":"completed"}}}`,
	}
	writeLog(t, root, "--Users-me-code-api--", testSession, events)
	s := newTestStore(t, root)

	page, err := s.Transcript(context.Background(), testSession, 0, 50)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}

	var notices []Item
	for _, item := range page.Items {
		if item.Role == RoleNotice {
			notices = append(notices, item)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("notices = %d, want exactly the settlement; items = %+v", len(notices), page.Items)
	}
	notice := notices[0]
	if notice.Actor != ActorSubagent {
		t.Errorf("actor = %q, want %q: the notice is about the child, not the session's agent", notice.Actor, ActorSubagent)
	}
	if notice.Outcome != OutcomeCompleted {
		t.Errorf("outcome = %q, want %q", notice.Outcome, OutcomeCompleted)
	}
	if want := "Background subagent " + child + " finished and will do no further work unless you send it more."; notice.Summary != want {
		t.Errorf("summary = %q, want the harness's own sentence %q", notice.Summary, want)
	}
	if notice.Text == "" {
		t.Error("text is empty; the row has no report to show")
	}

	// One relay, one settlement: the relay must not also appear, or the child's
	// report is shown twice.
	users := 0
	for _, item := range page.Items {
		if item.Role == RoleUser {
			users++
		}
	}
	if users != 1 {
		t.Errorf("user rows = %d, want 1: only the operator's own prompt is a prompt", users)
	}

	// A settlement is not the operator's opening line, so it must not become the
	// session's preview — which is what the session list shows in place of a
	// title.
	meta, err := s.Meta(context.Background(), testSession)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Preview != "review the branch" {
		t.Errorf("preview = %q, want the operator's prompt", meta.Preview)
	}
}

// TestSettlementOutcomeReadsTheHarnessVocabulary pins the mapping from the
// harness's six settlement sentences onto the outcome words a client colours by.
//
// The zero value matters as much as the cases: a wording this build has never
// seen is reported as *unknown*, never as completed. Reporting a refusal as a
// success is the one failure here that would actively mislead.
func TestSettlementOutcomeReadsTheHarnessVocabulary(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"Background subagent abc finished and will do no further work unless you send it more.", OutcomeCompleted},
		{"Background subagent abc was stopped before it finished.", OutcomeCancelled},
		{"Background subagent abc declined the task.", OutcomeFailed},
		{"Background subagent abc failed before it finished.", OutcomeFailed},
		{"Background subagent abc ran out of room before it finished.", OutcomeFailed},
		{"Background subagent abc ended abnormally (max-tokens) before it finished.", OutcomeFailed},
		{"Background subagent abc ended abnormally before it finished.", OutcomeFailed},
		{"something this build has never seen", ""},
	}
	for _, tc := range cases {
		if got := settlementOutcome(tc.text); got != tc.want {
			t.Errorf("settlementOutcome(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

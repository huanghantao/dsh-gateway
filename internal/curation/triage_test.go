package curation_test

import (
	"strings"
	"testing"

	"github.com/huanghantao/dsh-gateway/internal/curation"
)

// TestTriageKeepsWork is the half of triage that matters most: no amount of
// tidiness is worth suggesting that someone archive their own afternoon.
//
// The titles are invented, and deliberately so. An earlier revision used the
// titles one machine actually held, which made the test honest about the shapes
// a real operator produces and simultaneously published them. The shapes are
// what the rule is written against, so the shapes are what the fixtures keep:
// non-English questions, a task that mentions testing, a session that grew in a
// scratch directory, and a short session that is still unmistakably work.
func TestTriageKeepsWork(t *testing.T) {
	work := []curation.Session{
		{ID: "w1", Title: "把这份说明翻译成中文，语气随意", Workspace: "/Users/me/work", Messages: 38, LogReadable: true},
		{ID: "w2", Title: "配置文件的默认值在哪里定义", Workspace: "/Users/me/work/notes", Messages: 12, LogReadable: true},
		{ID: "w3", Title: "移动端布局方案讨论", Workspace: "/Users/me/work/notes", Messages: 40, LogReadable: true},
		{ID: "w4", Title: "数据导入模块重构", Workspace: "/Users/me/work/archive", Messages: 22, LogReadable: true},
		{ID: "w5", Title: "学习笔记整理成文档", Workspace: "/Users/me/study", Messages: 60, LogReadable: true},
		{ID: "w6", Title: "为什么会出现这个报错，分析一下", Workspace: "/Users/me/study", Messages: 9, LogReadable: true},
		{ID: "w7", Title: "把构建产物加进忽略列表", Workspace: "/Users/me/work/notes", Messages: 6, LogReadable: true},
		{ID: "w8", Title: "继续任务", Workspace: "/Users/me/work/notes", Messages: 120, LogReadable: true},
		// A title the operator wrote *about* tests is still their work.
		{ID: "w9", Title: "端到端测试为什么这么慢", Workspace: "/Users/me/work", Messages: 14, LogReadable: true},
		// A real conversation that happened to run in a scratch directory: it has
		// a title and content, so the guard protects it.
		{ID: "w10", Title: "调试内存占用过高", Workspace: "/tmp/scratch", Messages: 11, LogReadable: true},
	}
	for _, session := range work {
		if got := curation.Triage([]curation.Session{session}); len(got) != 0 {
			t.Errorf("triage suggested archiving real work %q: %+v", session.Title, got[0])
		}
	}
}

// TestTriageFindsTheNoise is the other half: the shapes an automated run leaves.
func TestTriageFindsTheNoise(t *testing.T) {
	tests := []struct {
		name    string
		session curation.Session
		verdict curation.Verdict
	}{
		{
			name:    "a script that dictates its own prompt",
			session: curation.Session{Title: "Reply with exactly: probe-ok", Workspace: "/Users/me/work", Messages: 5, LogReadable: true},
			verdict: curation.VerdictTest,
		},
		{
			name:    "a probe asking for a shell one-liner",
			session: curation.Session{Title: "Use the bash tool to run `echo probe`", Workspace: "/Users/me/work", Messages: 4, LogReadable: true},
			verdict: curation.VerdictTest,
		},
		{
			name:    "a fixture that names its own output",
			session: curation.Session{Title: "Run the bash tool with `sleep 4`, then reply with exactly: probe-17", Workspace: "/Users/me/work", Messages: 5, LogReadable: true},
			verdict: curation.VerdictTest,
		},
		{
			name:    "a go test's own session",
			session: curation.Session{Title: "", Workspace: "/var/folders/ty/xyz/T/TestSetConfigOption123/001", Messages: 4, LogReadable: true},
			verdict: curation.VerdictTemp,
		},
		{
			name:    "a session created and abandoned",
			session: curation.Session{Title: "", Workspace: "/Users/me/work", Messages: 0, LogReadable: true},
			verdict: curation.VerdictDraft,
		},
		{
			name:    "a prompt that was never answered",
			session: curation.Session{Title: "", Workspace: "/Users/me/work", Messages: 1, LogReadable: true},
			verdict: curation.VerdictDraft,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := curation.Triage([]curation.Session{tc.session})
			if len(got) != 1 {
				t.Fatalf("triage returned %d candidates, want 1", len(got))
			}
			if got[0].Verdict != tc.verdict {
				t.Errorf("verdict = %q, want %q", got[0].Verdict, tc.verdict)
			}
			if got[0].Reason == "" {
				t.Error("a candidate with no reason cannot be explained to the operator")
			}
		})
	}
}

// TestTriageKeepsUntitledButUsedSessions: a session with no title that holds a
// real conversation is not a draft, and is not anyone's to archive.
func TestTriageKeepsUntitledButUsedSessions(t *testing.T) {
	session := curation.Session{Title: "", Workspace: "/Users/me/work", Messages: 12, LogReadable: true}
	if got := curation.Triage([]curation.Session{session}); len(got) != 0 {
		t.Errorf("an untitled session with 12 messages was called %q", got[0].Verdict)
	}
}

// TestTriageIsPerSession keeps one session's verdict from spilling onto others.
func TestTriageIsPerSession(t *testing.T) {
	candidates := curation.Triage([]curation.Session{
		{ID: "noise", Title: "Reply with exactly: probe-ok", Workspace: "/Users/me/work", Messages: 3, LogReadable: true},
		{ID: "work", Title: "把这份说明翻译成中文", Workspace: "/Users/me/work", Messages: 38, LogReadable: true},
	})
	if len(candidates) != 1 || candidates[0].ID != "noise" {
		t.Fatalf("candidates = %+v, want only the test run", candidates)
	}
}

// TestTriageNeedsEvidenceNotSilence is the rule that keeps a misconfiguration
// from becoming a mass archive: a session whose history could not be read has no
// title and no messages, which is indistinguishable from a draft unless the
// caller says so.
func TestTriageNeedsEvidenceNotSilence(t *testing.T) {
	unreadable := []curation.Session{
		{ID: "unreadable-1", Workspace: "/Users/me/work"},
		{ID: "unreadable-2", Workspace: "/Users/me/work"},
	}
	if got := curation.Triage(unreadable); len(got) != 0 {
		t.Errorf("triage suggested archiving %d session(s) it could not read", len(got))
	}

	// A scratch directory is evidence in itself — it comes from the listing, not
	// from the log — so that rule still applies.
	scratch := []curation.Session{{ID: "scratch", Workspace: "/tmp/whatever"}}
	if got := curation.Triage(scratch); len(got) != 1 || got[0].Verdict != curation.VerdictTemp {
		t.Errorf("an unreadable session in a scratch directory was not recognised: %+v", got)
	}
}

// TestRulesAreExtensible covers the escape hatch for a harness whose markers are
// not the shipped ones. Without it the portable rules are the only rules, and an
// operator whose test runner says something else has no way to teach it.
func TestRulesAreExtensible(t *testing.T) {
	session := curation.Session{
		Title:       "wafl-probe: report the tile count",
		Workspace:   "/Users/me/work",
		Messages:    3,
		LogReadable: true,
	}

	if got := curation.Triage([]curation.Session{session}); len(got) != 0 {
		t.Fatalf("the shipped rules matched a marker they should not know about: %+v", got)
	}

	rules, err := curation.DefaultRules().WithPatterns([]string{`wafl-probe`})
	if err != nil {
		t.Fatalf("WithPatterns: %v", err)
	}
	got := curation.TriageWith([]curation.Session{session}, rules)
	if len(got) != 1 || got[0].Verdict != curation.VerdictTest {
		t.Fatalf("a configured marker was not honoured: %+v", got)
	}

	// The built-in shapes must survive the merge: teaching triage one marker
	// cannot be allowed to unlearn the portable ones.
	builtin := curation.Session{Title: "Reply with exactly: probe-ok", Workspace: "/Users/me/work", Messages: 3, LogReadable: true}
	if got := curation.TriageWith([]curation.Session{builtin}, rules); len(got) != 1 {
		t.Errorf("adding a pattern dropped the built-in shapes: %+v", got)
	}

	if _, err := curation.DefaultRules().WithPatterns([]string{`(`}); err == nil {
		t.Error("an uncompilable pattern was accepted")
	} else if !strings.Contains(err.Error(), "test title pattern") {
		t.Errorf("error = %v, want it to name the offending setting", err)
	}
}

// TestTempRootsArePlatformAware: a scratch directory only counts if it is one,
// and which directories those are is a property of the machine, not of the rule.
func TestTempRootsArePlatformAware(t *testing.T) {
	rules := curation.DefaultRules()
	if len(rules.TempRoots) == 0 {
		t.Fatal("the default rules have no temp roots, so the temp verdict can never fire")
	}
	// os.TempDir() is the one entry that is guaranteed to be right here, and it
	// is what a hardcoded macOS-shaped list would have missed on Linux.
	if tmp := t.TempDir(); tmp != "" {
		custom := rules
		custom.TempRoots = append(append([]string(nil), rules.TempRoots...), tmp)
		session := curation.Session{Workspace: tmp, Messages: 2}
		got := curation.TriageWith([]curation.Session{session}, custom)
		if len(got) != 1 || got[0].Verdict != curation.VerdictTemp {
			t.Errorf("a configured temp root was not consulted: %+v", got)
		}
	}
}

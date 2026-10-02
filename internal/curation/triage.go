package curation

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Session is what triage needs to know about one session. It is deliberately not
// the API's view type: the rules are about sessions, not about a wire format.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Messages  int    `json:"messages,omitempty"`
	// LogReadable is whether the caller could actually read this session's
	// history. Two of the rules below are about *content* — a title that reads
	// like a script, a session with nothing in it — and no content is not the
	// same as empty content. Without this flag, a gateway whose transcript
	// projection is off would see every session as a draft and offer to archive
	// the operator's entire history.
	LogReadable bool `json:"-"`
}

// Verdict is why a session looks like it can be put away.
type Verdict string

const (
	// VerdictTest is a session whose title reads like an automated run.
	VerdictTest Verdict = "test"
	// VerdictTemp is a session in a directory nobody keeps work in.
	VerdictTemp Verdict = "temp"
	// VerdictDraft is a session that was created and never used.
	VerdictDraft Verdict = "draft"
)

// Candidate is one session triage suggests putting away, and why.
type Candidate struct {
	Session
	Verdict Verdict `json:"verdict"`
	Reason  string  `json:"reason"`
}

// Candidates is the answer to "what could be tidied", with the counts a UI needs
// to say how much of the list is noise before the operator opens anything.
type Candidates struct {
	Scanned int `json:"scanned"`
	// Candidates is how many sessions the rules would put away, which is the
	// number a banner leads with.
	Candidates int `json:"candidates"`
	// Archived counts sessions already put away, so a summary can be honest about
	// what it did not look at.
	Archived int `json:"archived"`
	Test     int `json:"test"`
	Temp     int `json:"temp"`
	Draft    int `json:"draft"`
}

// defaultTestTitles matches the shapes an automated run leaves behind.
//
// These are not guesses about the operator's naming: they are the sentences a
// test *has* to say to be a test — "reply with exactly", a shell one-liner a
// script asked for. A session the operator wrote themselves reads like a
// question, not like an instruction to a robot.
//
// What is deliberately absent is any marker belonging to one particular test
// harness. An earlier revision carried the names this project's own browser test
// emits, which meant the shipped rule knew about a private script: harmless here,
// useless to anyone else, and wrong to ship. An operator whose harness says
// something else adds it through curation.testTitlePatterns.
var defaultTestTitles = regexp.MustCompile(`(?i)(` +
	`reply with|use the bash tool|run the bash|run ` + "`" + `|echo [a-z0-9_\-]+|` +
	`say hello|count from 1 to 5|write the exact text|test session|` +
	`reply test|tool test` +
	`)`)

// How long a session may be and still be treated as a run rather than a
// conversation.
//
// These are the guard rails. An automated run is short by nature — a prompt, an
// answer, a tool call or two — while a session the operator actually worked in
// grows, so length is what separates "a test that happened to be long" from "a
// conversation that must not be touched". Set them generously: missing a noisy
// session costs one manual archive, archiving someone's afternoon costs trust.
const (
	tempCap = 8
	testCap = 12
)

// defaultTempRoots are directories that hold scratch work by definition.
//
// The list is the intersection of what is true on every platform this runs on,
// plus whatever the runtime reports for this one; see Rules.TempRoots.
var defaultTempRoots = []string{"/var/folders", "/private/tmp", "/tmp", "/private/var/folders", "/var/tmp"}

// Rules decides what counts as a run rather than a conversation.
//
// The zero value is not useful; start from DefaultRules and replace what the
// deployment knows better. Both fields exist because "a test" is a local
// definition: a marker that identifies one operator's harness means nothing on
// another machine, and a scratch directory that exists on macOS may not exist on
// Linux.
type Rules struct {
	// TestTitles matches titles an automated run leaves behind. Nil means no
	// title is ever taken as evidence of a run.
	TestTitles *regexp.Regexp
	// TempRoots are path prefixes that hold scratch work. A session here is only
	// ever a *candidate*; length still has to agree.
	TempRoots []string
}

// DefaultRules is the portable starting point: the generic instruction shapes,
// the scratch directories this machine actually has, and nothing specific to any
// one person's setup.
func DefaultRules() Rules {
	roots := append([]string(nil), defaultTempRoots...)
	if tmp := os.TempDir(); tmp != "" {
		roots = append(roots, tmp)
	}
	if runtime.GOOS == "windows" {
		if tmp := os.Getenv("TEMP"); tmp != "" {
			roots = append(roots, tmp)
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			roots = append(roots, filepath.Join(local, "Temp"))
		}
	}
	return Rules{TestTitles: defaultTestTitles, TempRoots: dedupe(roots)}
}

// WithPatterns returns a copy of the rules whose title evidence is the default
// set plus extra, so a deployment can teach triage its own harness's noise
// without losing the portable shapes. An uncompilable extra is ignored: a bad
// pattern should cost a candidate, not a panic on the sessions screen.
func (r Rules) WithPatterns(extra []string) (Rules, error) {
	kept := make([]string, 0, len(extra))
	for _, pattern := range extra {
		if strings.TrimSpace(pattern) == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return r, fmt.Errorf("curation: test title pattern %q: %w", pattern, err)
		}
		kept = append(kept, pattern)
	}
	if len(kept) == 0 {
		return r, nil
	}
	combined := `(?i)(` + strings.Join(append([]string{defaultTestTitles.String()}, kept...), "|") + `)`
	compiled, err := regexp.Compile(combined)
	if err != nil {
		return r, fmt.Errorf("curation: combined test title pattern: %w", err)
	}
	r.TestTitles = compiled
	return r, nil
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// Triage classifies sessions that are safe to suggest for archiving, using the
// portable default rules.
func Triage(sessions []Session) []Candidate { return TriageWith(sessions, DefaultRules()) }

// TriageWith classifies sessions under an explicit rule set.
//
// The bias is deliberate and asymmetric: a false negative costs the operator one
// tap (they archive it themselves), while a false positive puts their work in a
// list they are about to bulk-archive. So a session is only ever a candidate
// when the evidence is structural — where it lives, what an automated run said,
// whether anything was ever said at all — and never when it merely looks quiet.
func TriageWith(sessions []Session, rules Rules) []Candidate {
	candidates := make([]Candidate, 0, len(sessions))
	for _, session := range sessions {
		verdict, reason, ok := classify(session, rules)
		if !ok {
			continue
		}
		candidates = append(candidates, Candidate{Session: session, Verdict: verdict, Reason: reason})
	}
	return candidates
}

// classify returns the verdict for one session, if it has one.
func classify(session Session, rules Rules) (Verdict, string, bool) {
	titled := strings.TrimSpace(session.Title) != ""

	// A scratch directory holds scratch work, but a conversation that grew there
	// is still a conversation.
	if isTempWorkspace(session.Workspace, rules.TempRoots) && session.Messages < tempCap {
		return VerdictTemp, "created in a temporary directory", true
	}
	// The title is the strongest signal there is: these are sentences a script
	// says to an agent, so no amount of length makes them someone's own work.
	if rules.TestTitles != nil && session.LogReadable &&
		rules.TestTitles.MatchString(session.Title) && session.Messages < testCap {
		return VerdictTest, "the title reads like an automated run", true
	}
	// A draft is a session with nothing in it: no title, and no conversation to
	// speak of. One message is the prompt that never got an answer.
	if session.LogReadable && !titled && session.Messages <= 1 {
		return VerdictDraft, "never used", true
	}
	return "", "", false
}

// isTempWorkspace reports whether a workspace is scratch space.
func isTempWorkspace(workspace string, roots []string) bool {
	if workspace == "" {
		return false
	}
	cleaned := filepath.Clean(workspace)
	for _, root := range roots {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		if cleaned == root || strings.HasPrefix(cleaned, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

package sessionlog

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"
)

// Match is one place a query was found.
type Match struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Role      string `json:"role,omitempty"`
	Tool      string `json:"tool,omitempty"`
	// Field names which part of the row matched: "text", "tool", "input" or
	// "output".
	//
	// It exists because those four mean different things to a reader. A hit in
	// "output" is the answer to "which session printed this stack trace", and a
	// result list that did not distinguish it from something the operator typed
	// would send them looking in the wrong place.
	Field string `json:"field,omitempty"`
	// Snippet is the text around the match, trimmed to something readable on a
	// phone. It is what makes a result recognisable: a list of session titles
	// says where a phrase might be, not where it is.
	Snippet string    `json:"snippet"`
	Time    time.Time `json:"time,omitempty"`
}

// SearchOptions bounds a search.
type SearchOptions struct {
	// Sessions is how many sessions to scan, newest first. Searching is the one
	// operation that cannot be answered from a cache — the text has to be read —
	// so it is bounded rather than exhaustive, and the bound is reported.
	Sessions int
	// PerSession caps how many matches one session contributes, so a single long
	// conversation cannot fill the result list.
	PerSession int
	// Matches caps the whole result.
	Matches int
	// Workspace, when set, restricts the search to one workspace.
	Workspace string
}

// DefaultSearchOptions are the bounds a phone search uses: enough of the recent
// history to answer "where did I ask about that", without reading every session
// ever recorded.
func DefaultSearchOptions() SearchOptions {
	return SearchOptions{Sessions: 120, PerSession: 3, Matches: 50}
}

// Search looks for text inside session transcripts.
//
// It is deliberately a scan and not an index. An index would have to be built,
// persisted, invalidated when the desktop writes to a log this process is not
// watching, and rebuilt when a log is deleted — real machinery, for a query a
// reader makes a few times a day over a corpus that fits in a few hundred
// megabytes. The bound in SearchOptions is what keeps that honest.
func (s *Store) Search(ctx context.Context, query string, opts SearchOptions) (matches []Match, scanned int, err error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, 0, nil
	}
	if opts.Sessions <= 0 {
		opts.Sessions = DefaultSearchOptions().Sessions
	}
	if opts.PerSession <= 0 {
		opts.PerSession = DefaultSearchOptions().PerSession
	}
	if opts.Matches <= 0 {
		opts.Matches = DefaultSearchOptions().Matches
	}

	// Newest first: the thing someone is trying to remember is almost always
	// recent, and a bounded search should spend its budget where it will be paid
	// back.
	type candidate struct {
		id   string
		path string
	}
	logs := s.Logs()
	candidates := make([]candidate, 0, len(logs))
	for _, log := range logs {
		candidates = append(candidates, candidate{id: log.ID, path: log.Path})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := s.modTime(candidates[i].path), s.modTime(candidates[j].path)
		return left.After(right)
	})

	for _, candidate := range candidates {
		if scanned >= opts.Sessions || len(matches) >= opts.Matches {
			break
		}
		if err := ctx.Err(); err != nil {
			return matches, scanned, err
		}
		scanned++

		items, meta, err := s.Items(ctx, candidate.id)
		if err != nil {
			// A session that cannot be read is not a reason to fail a search over
			// the ones that can.
			continue
		}
		if opts.Workspace != "" && meta.Workspace != opts.Workspace {
			continue
		}

		found := 0
		for _, item := range items {
			if found >= opts.PerSession || len(matches) >= opts.Matches {
				break
			}
			hit, ok := matchItem(item, needle)
			if !ok {
				continue
			}
			found++
			matches = append(matches, Match{
				SessionID: candidate.id,
				Title:     meta.Title,
				Workspace: meta.Workspace,
				Seq:       item.Seq,
				Role:      string(item.Role),
				Tool:      item.Tool,
				Field:     hit.field,
				Snippet:   snippet(hit.text, hit.index, len(needle)),
				Time:      item.Time,
			})
		}
	}
	return matches, scanned, nil
}

// modTime reads a log's mtime, tolerating a file that went away mid-search.
func (s *Store) modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// hit is where a query was found inside one item.
type hit struct {
	field string
	text  string
	index int
}

// matchItem looks for needle in the parts of an item worth searching, in the
// order a reader would look: what was said, what was run, what it printed.
//
// Tool output is included deliberately. "Which session printed that stack trace"
// is the question a coding agent's history is most often asked, and it is only
// answerable from the output — the call that produced it says nothing about what
// came back. Searching the concatenation instead would report the right row with
// a snippet taken from the wrong field, which is why each part is tried on its
// own.
func matchItem(item Item, needle string) (hit, bool) {
	fields := [...]struct {
		name string
		text string
	}{
		{"text", item.Text},
		{"tool", item.Tool},
		{"input", item.Input},
		{"output", item.Output},
	}
	for _, f := range fields {
		if f.text == "" {
			continue
		}
		if index := strings.Index(strings.ToLower(f.text), needle); index >= 0 {
			return hit{field: f.name, text: f.text, index: index}, true
		}
	}
	return hit{}, false
}

// snippetWidth is how much text a result shows around the match.
const snippetWidth = 60

// markdownMarkers are the characters that only make sense when rendered.
var markdownMarkers = strings.NewReplacer("**", "", "__", "", "`", "", "### ", "", "## ", "", "# ", "")

// snippet cuts a readable window around a match.
//
// Runes, not bytes: this product's text is mostly Chinese, and a byte-wise cut
// would produce a snippet of broken characters — worse than no snippet at all,
// because it looks like the session contains mojibake.
func snippet(text string, byteIndex, byteLength int) string {
	runes := []rune(text)
	// Convert the byte offset into a rune offset.
	runeIndex := len([]rune(text[:byteIndex]))
	matchRunes := len([]rune(text[byteIndex : byteIndex+byteLength]))

	start := runeIndex - snippetWidth/2
	if start < 0 {
		start = 0
	}
	end := runeIndex + matchRunes + snippetWidth/2
	if end > len(runes) {
		end = len(runes)
	}

	out := strings.TrimSpace(string(runes[start:end]))
	out = strings.Join(strings.Fields(out), " ")
	// Messages are Markdown, and a snippet is read as plain text: leaving the
	// asterisks in makes the result look like it contains mojibake rather than
	// emphasis.
	out = markdownMarkers.Replace(out)
	if start > 0 {
		out = "…" + out
	}
	if end < len(runes) {
		out += "…"
	}
	return out
}

package sessionlog

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// Receipt is what a session actually did.
//
// A conversation answers "what was said"; this answers "what happened" — which
// is the question someone asks a day later, when they remember the agent worked
// on something and want to know what it cost and what it touched. Everything
// here is derived from the log that is already on disk, so it needs no
// bookkeeping of its own and cannot drift from the conversation it describes.
type Receipt struct {
	ID        string `json:"id"`
	Title     string `json:"title,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Model     string `json:"model,omitempty"`
	// Effort is the reasoning effort the session ran at, from its last model
	// selection: part of what it cost, and recorded nowhere else.
	Effort string `json:"reasoningEffort,omitempty"`

	// Messages counts what a conversation shows: prompts and answers.
	Messages int `json:"messages"`
	Turns    int `json:"turns"`

	// Tools is what the agent ran, most used first.
	Tools []ToolUse `json:"tools,omitempty"`
	// Files are the paths it read or wrote, in the order it first touched them.
	Files []string `json:"files,omitempty"`

	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
	// ContextTokens is the size of the context at the last message, which is what
	// the next turn would have had to carry.
	ContextTokens int `json:"contextTokens,omitempty"`

	// StartedAt and EndedAt bound the session's life, which includes however long
	// it sat idle between prompts.
	StartedAt time.Time `json:"startedAt,omitempty"`
	EndedAt   time.Time `json:"endedAt,omitempty"`
	// SpanSeconds is that whole wall-clock life.
	SpanSeconds int `json:"spanSeconds"`
	// Cost is present only when a price is configured for the model this session
	// ran; a made-up number would be worse than none.
	Cost *Cost `json:"cost,omitempty"`

	// ActiveSeconds is time the agent was demonstrably doing something: the gaps
	// between consecutive log entries, with long gaps discarded. It is an
	// estimate and says so — a turn that spent four minutes inside one tool call
	// looks like one gap, and a session left open overnight does not look like
	// eight hours of work.
	ActiveSeconds int `json:"activeSeconds"`
}

// Cost is an estimate built from configured unit prices.
type Cost struct {
	Currency string  `json:"currency"`
	Total    float64 `json:"total"`
	// The parts, so the arithmetic is visible rather than asserted: a reader who
	// disagrees with the price can see exactly what was multiplied.
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cacheRead"`
}

// ToolUse is one tool's tally.
type ToolUse struct {
	Name   string `json:"name"`
	Calls  int    `json:"calls"`
	Failed int    `json:"failed"`
}

// idleGap is the longest gap counted as work.
//
// A tool call that takes longer than this is indistinguishable from a session
// nobody was looking at, and counting the second as work is how a "3 hours"
// figure loses its meaning.
const idleGap = 5 * time.Minute

// Price is a unit price per million tokens.
type Price struct {
	Model     string
	Currency  string
	Input     float64
	Output    float64
	CacheRead float64
}

// Summarise builds a receipt from a session's transcript.
func Summarise(items []Item, meta Meta) Receipt {
	receipt := Receipt{
		ID:        meta.ID,
		Title:     meta.Title,
		Workspace: meta.Workspace,
		Model:     meta.Model,
		Effort:    meta.Effort,
		Turns:     meta.TurnCount,
		StartedAt: meta.CreatedAt,
		EndedAt:   meta.UpdatedAt,
	}
	if receipt.ID == "" {
		receipt.ID = meta.ID
	}

	calls := map[string]*ToolUse{}
	seenFile := map[string]bool{}
	var previous time.Time

	for _, item := range items {
		switch item.Role {
		case RoleUser, RoleAssistant:
			receipt.Messages++
		case RoleNotice:
			// Turn bookkeeping — "turn started", "turn cancelled" — not something
			// the operator said or the agent did. It contributes no message, no
			// tool call and no file, which is why it has a case of its own rather
			// than falling through the switch silently.
		case RoleTool:
			use, ok := calls[item.Tool]
			if !ok {
				use = &ToolUse{Name: item.Tool}
				calls[item.Tool] = use
			}
			use.Calls++
			if item.IsError {
				use.Failed++
			}
			for _, path := range touchedFiles(item.Tool, item.Input) {
				if seenFile[path] {
					continue
				}
				seenFile[path] = true
				receipt.Files = append(receipt.Files, path)
			}
		}

		if item.Usage != nil {
			receipt.InputTokens += item.Usage.InputTokens
			receipt.OutputTokens += item.Usage.OutputTokens
			receipt.CacheReadTokens += item.Usage.CacheReadTokens
			if item.Usage.TotalTokens > 0 {
				receipt.ContextTokens = item.Usage.TotalTokens
			}
		}

		if !item.Time.IsZero() {
			if !previous.IsZero() {
				if gap := item.Time.Sub(previous); gap > 0 && gap <= idleGap {
					receipt.ActiveSeconds += int(gap.Seconds())
				}
			}
			previous = item.Time
		}
	}

	// The log's own timestamps win over the store's fallbacks. `Meta` fills
	// created/updated from the file's mtime when the log does not carry them,
	// which is the right answer for sorting a list and the wrong one for a
	// receipt: "started" is when the first thing in the log happened.
	if len(items) > 0 {
		if !items[0].Time.IsZero() {
			receipt.StartedAt = items[0].Time
		}
		if last := items[len(items)-1].Time; !last.IsZero() {
			receipt.EndedAt = last
		}
	}
	if !receipt.StartedAt.IsZero() && receipt.EndedAt.After(receipt.StartedAt) {
		receipt.SpanSeconds = int(receipt.EndedAt.Sub(receipt.StartedAt).Seconds())
	}

	receipt.Tools = make([]ToolUse, 0, len(calls))
	for _, use := range calls {
		receipt.Tools = append(receipt.Tools, *use)
	}
	sort.Slice(receipt.Tools, func(i, j int) bool {
		if receipt.Tools[i].Calls != receipt.Tools[j].Calls {
			return receipt.Tools[i].Calls > receipt.Tools[j].Calls
		}
		return receipt.Tools[i].Name < receipt.Tools[j].Name
	})

	return receipt
}

// WithCost attaches an estimate, when a price for this session's model is known.
//
// The three token counts are disjoint — fresh prompt, cached prompt, and output —
// which is what makes this arithmetic rather than a guess. Prices are per
// million, the way every provider quotes them.
func (r Receipt) WithCost(prices []Price) Receipt {
	price, ok := matchPrice(prices, r.Model)
	// An all-zero entry is what a template ships, and "≈ ¥0.00" would read as
	// "this session was free" rather than "nobody has filled this in".
	if !ok || (price.Input == 0 && price.Output == 0 && price.CacheRead == 0) {
		return r
	}
	cost := Cost{
		Currency:  price.Currency,
		Input:     float64(r.InputTokens) / 1e6 * price.Input,
		Output:    float64(r.OutputTokens) / 1e6 * price.Output,
		CacheRead: float64(r.CacheReadTokens) / 1e6 * price.CacheRead,
	}
	cost.Total = cost.Input + cost.Output + cost.CacheRead
	r.Cost = &cost
	return r
}

// matchPrice finds the price for a model, tolerating a provider prefix.
//
// Sessions record whichever name the client sent — "deepseek-v4.1-flash" or
// "deepseek/deepseek-v4.1-flash" — and a table that silently failed to match
// would look like a session that cost nothing.
func matchPrice(prices []Price, model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	short := model
	if index := strings.LastIndex(model, "/"); index >= 0 {
		short = model[index+1:]
	}
	for _, price := range prices {
		if price.Model == model {
			return price, true
		}
	}
	for _, price := range prices {
		candidate := price.Model
		if index := strings.LastIndex(candidate, "/"); index >= 0 {
			candidate = candidate[index+1:]
		}
		if candidate == short {
			return price, true
		}
	}
	return Price{}, false
}

// pathTools are the tools whose first argument is a file they touched.
var pathTools = map[string][]string{
	"read":       {"file_path", "path"},
	"write":      {"file_path", "path"},
	"edit":       {"file_path", "path"},
	"read_image": {"path", "file_path"},
}

// touchedFiles reads the paths out of a tool call's arguments.
//
// Deliberately forgiving: the arguments are a JSON string written by a tool this
// package does not own, and a receipt that failed to parse one call would be
// worse than a receipt that missed a file.
func touchedFiles(tool, input string) []string {
	keys, ok := pathTools[tool]
	if !ok || input == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return nil
	}
	for _, key := range keys {
		if path, ok := args[key].(string); ok && path != "" {
			return []string{path}
		}
	}
	return nil
}

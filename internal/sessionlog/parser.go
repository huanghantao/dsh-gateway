package sessionlog

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// parser folds session log events into transcript items.
//
// It is a single-pass, streaming fold: the log is append-only and can be
// megabytes, so nothing is retained beyond the items it emits and a small map
// correlating tool calls with their results.
//
// A parser lives as long as the fold it belongs to, which for a cached session
// is as long as the cache entry: a later read feeds it the events that arrived
// since, so it must never be handed the same line twice. What it has already
// seen is what makes the projection cumulative.
type parser struct {
	sessionID string
	logger    *logx.Logger

	// keepItems is false when only the metadata is wanted — the session list
	// reads every session in the store, and the rows of a session nobody opened
	// are not worth holding. The fold is otherwise identical, so both readers
	// agree on titles, counts and previews by construction.
	keepItems bool

	items []Item
	meta  Meta

	// toolIndex maps a tool call id to its position in items, so the result
	// event can complete the item the call event started. The result carries no
	// tool name, so this correlation is the only way to label it. It is only
	// populated when items are kept.
	toolIndex map[string]int

	// head is true until the first line is read, for a parser that starts at the
	// beginning of a log. The header line is what says the file is a log this
	// build can read at all, so it is checked exactly once, by the parser that
	// owns that end of the file — and never by one resuming in the middle of it.
	head bool
	// headerErr is the header's verdict, reported by finish. A parser that
	// rejected its header ignores every line after it: the rest of the file
	// cannot be interpreted, so nothing in it may reach the transcript.
	headerErr error
}

func newParser(sessionID string, logger *logx.Logger, keepItems bool) *parser {
	return &parser{
		sessionID: sessionID,
		logger:    logger,
		keepItems: keepItems,
		meta:      Meta{ID: sessionID},
		toolIndex: map[string]int{},
		head:      true,
	}
}

// metaParser builds the parser that continues a metadata-only fold: the metadata
// folded so far, and the resume point it stopped at.
//
// In this mode a parser retains nothing but the metadata, which the record
// already holds, so the state to resume from is the value itself rather than a
// parser kept alive for every session in the store. An empty resume point means
// the read starts at the beginning of the log, and that is where a header is
// checked — having folded anything at all is what says a resume is past it.
func metaParser(sessionID string, logger *logx.Logger, meta Meta, from progress) *parser {
	if meta.ID == "" {
		meta.ID = sessionID
	}
	return &parser{
		sessionID: sessionID,
		logger:    logger,
		meta:      meta,
		head:      from.empty(),
	}
}

// checkHeader validates the first line of a log.
func checkHeader(line []byte) error {
	var header struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal(line, &header); err != nil {
		return errx.Wrap(ErrUnsupportedFormat, errx.KindInternal,
			"transcript_unsupported", "the stored transcript is not in a recognised format")
	}
	if header.Type != "session" {
		return errx.New(errx.KindInternal, "transcript_unsupported",
			"the stored transcript does not begin with a session header")
	}
	if header.Version != SupportedVersion {
		return fmt.Errorf("%w: file is version %d, this build understands %d",
			ErrUnsupportedFormat, header.Version, SupportedVersion)
	}
	return nil
}

// envelope decodes just enough of an event to route it.
type envelope struct {
	Type string `json:"type"`
	Seq  int64  `json:"seq"`
	Time int64  `json:"time"`
}

// feed consumes one log line.
func (p *parser) feed(line []byte) {
	if p.headerErr != nil {
		// A log whose header this build cannot read is not interpreted further:
		// events from a newer schema would be folded on guesses.
		return
	}
	if p.head {
		p.head = false
		if err := checkHeader(line); err != nil {
			p.headerErr = err
			return
		}
	}

	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		// A corrupt line should not cost the operator the rest of their history.
		p.debug("skipping unparseable event", err)
		return
	}

	switch env.Type {
	case "session":
		p.feedSession(line)
	case "session/title":
		p.feedTitle(line)
	case "model/selection", "request/header":
		p.feedModel(line)
	case "user/message":
		p.feedUserMessage(line, env)
	case "agent/inbox/spliced":
		p.feedInboxSplice(line, env)
	case "assistant/message":
		p.feedAssistantMessage(line, env)
	case "tool/call":
		p.feedToolCall(line, env)
	case "tool/result":
		p.feedToolResult(line, env)
	case "turn/start":
		// Recorded so a follower can tell a session that is working right now
		// from one that is merely recent.
		p.meta.TurnRunning = true
	case "turn/end":
		p.meta.TurnRunning = false
		p.meta.TurnCount++

	default:
		// Everything else — step boundaries, sandbox and approval policy
		// records — is internal bookkeeping that a phone transcript has no use
		// for. Skipping unknown types rather than rejecting them is what lets
		// DSH add new event kinds without breaking history rendering.
	}
}

// injectedMessage is one message DSH splices into a session's inbox.
//
// The harness delivers more than the operator's prompts this way: a delegated
// child's final report, a settlement notice, a runtime-context snapshot. The
// `source` object is what tells them apart, and it is the reason none of this
// has to be guessed at from the prose.
type injectedMessage struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Source *struct {
		Kind string `json:"kind"`
		// Summary is the harness's own one-line account of the notice.
		Summary string `json:"summary"`
	} `json:"source"`
	ID string `json:"id"`
}

// feedInboxSplice reads an `agent/inbox/spliced` event for the one thing in it a
// reader needs: a delegated task's settlement.
//
// This is where the gateway used to lose the answer to "which agent finished,
// and how did it go?". DSH records a settlement with a typed source —
// `kind: "subagent-settled"`, `form: "notice"`, its own `summary` sentence and
// the child's session id — and the projection dropped the whole event as
// bookkeeping, so a phone watching a desk session was never told a child had
// finished at all.
//
// Everything else a splice carries is skipped on purpose. A relay is the child's
// report delivered *to the model*, and the same text follows the settlement as
// its closing message; projecting both would show one child's answer twice,
// which is a duplication DSH's own issue tracker already records (dsh#6744).
func (p *parser) feedInboxSplice(line []byte, env envelope) {
	var ev struct {
		Data struct {
			Inserted []injectedMessage `json:"inserted"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("inbox splice", err)
		return
	}
	for index, message := range ev.Data.Inserted {
		if message.Source == nil || message.Source.Kind != sourceSubagentSettled {
			continue
		}
		// The blocks are joined the way the harness assembled them, which is why
		// its sentence and its "Its closing message:" marker run together without
		// a separating newline.
		text := joinedContent(message)
		if strings.TrimSpace(text) == "" {
			continue
		}
		id := message.ID
		if id == "" {
			// A splice carries no id of its own in the logs this build has seen.
			// The sequence number and the position within the splice are stable
			// across reads, which is what a key has to be.
			id = fmt.Sprintf("splice-%d-%d", env.Seq, index)
		}
		p.append(Item{
			ID:   id,
			Seq:  env.Seq,
			Time: fromMillis(env.Time),
			Role: RoleNotice,
			Text: text,
			// The actor is the *child*, named by the harness, and never the main
			// agent: a settlement is the one notice in a session that is not
			// about the session's own agent.
			Actor:   ActorSubagent,
			Summary: strings.TrimSpace(message.Source.Summary),
			Outcome: settlementOutcome(text),
		})
	}
}

// joinedContent concatenates the text blocks of one injected message.
//
// The log stores a settlement as several blocks — the sentence, the "Its closing
// message:" marker, the report — and they are one message to every reader of it,
// which is how the harness assembled them and how the ACP path delivers them.
func joinedContent(message injectedMessage) string {
	var text strings.Builder
	for _, block := range message.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}

// settlementOutcome reads how a settled child ended, from the harness's own six
// sentences. It exists because the source object records the fact that a child
// settled but not how it went; getting that wrong would report a refusal as a
// success, so an unrecognised wording is reported as an unknown outcome rather
// than assumed to be completion.
func settlementOutcome(text string) string {
	switch {
	case strings.Contains(text, "finished and will do no further work"),
		strings.Contains(text, "finished and left no closing message"):
		return OutcomeCompleted
	case strings.Contains(text, "was stopped before it finished"):
		return OutcomeCancelled
	case strings.Contains(text, "declined the task"),
		strings.Contains(text, "failed before it finished"),
		strings.Contains(text, "ran out of room before it finished"),
		strings.Contains(text, "ended abnormally"):
		return OutcomeFailed
	default:
		return ""
	}
}

func (p *parser) feedSession(line []byte) {
	var ev struct {
		ID        string `json:"id"`
		CreatedAt int64  `json:"createdAt"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("session header", err)
		return
	}
	if ev.ID != "" {
		p.meta.ID = ev.ID
	}
	p.meta.Workspace = ev.Cwd
	p.meta.CreatedAt = fromMillis(ev.CreatedAt)
}

func (p *parser) feedTitle(line []byte) {
	var ev struct {
		Data struct {
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("session title", err)
		return
	}
	if ev.Data.Title != "" {
		p.meta.Title = ev.Data.Title
	}
}

// feedModel records the route a session actually ran on.
//
// Two event shapes carry it, and reading only one of them is why a session the
// gateway is not holding showed no model at all:
//
//   - `model/selection` is appended by DSH's web session controller, so it
//     exists only for a session whose model was changed from the desktop GUI.
//   - `request/header` is appended by the agent loop for every request series
//     whose canonical config changed, and it is the only place a session driven
//     over ACP — every session this gateway creates — records its route.
//
// A header repeats whenever the route changes, so the last one wins, which is
// exactly what "the model this session is using" means.
func (p *parser) feedModel(line []byte) {
	var ev struct {
		Data struct {
			// `model/selection` carries the selection at the top level.
			Provider        string `json:"provider"`
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoningEffort"`
			// `request/header` nests the resolved config under the header it
			// sent, alongside the tool catalogue.
			Header struct {
				Config struct {
					Provider        string `json:"provider"`
					Model           string `json:"model"`
					ReasoningEffort string `json:"reasoningEffort"`
				} `json:"config"`
			} `json:"header"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("model selection", err)
		return
	}

	model, effort := ev.Data.Model, ev.Data.ReasoningEffort
	if ev.Data.Header.Config.Model != "" {
		model, effort = ev.Data.Header.Config.Model, ev.Data.Header.Config.ReasoningEffort
	}
	if model != "" {
		p.meta.Model = model
	}
	// The effort in force is part of what a session cost, and this event is the
	// only place the log records it.
	if effort != "" {
		p.meta.Effort = effort
	}
}

// contentBlock is one element of a message body. The same shape is used for user
// and assistant messages; only the block types present differ.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// attachments counts the non-text blocks in a content array.
//
// It counts rather than carries: see Item.Attachments. Any unrecognised type is
// counted too, because from a reader's point of view "something was attached
// that is not words" is the fact worth showing, whatever the harness called it.
func attachments(blocks []contentBlock) int {
	n := 0
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" && b.Type != "reasoning" {
			n++
		}
	}
	return n
}

func (p *parser) feedUserMessage(line []byte, env envelope) {
	var ev struct {
		Data struct {
			Content []contentBlock `json:"content"`
			ID      string         `json:"id"`
			Source  struct {
				Kind string `json:"kind"`
			} `json:"source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("user message", err)
		return
	}

	// Only a message the operator actually typed becomes a bubble.
	//
	// DSH records its own injections in this same event type, and they are not
	// conversation: a runtime-context snapshot describing the sandbox policy, and
	// a skill catalog listing every installed skill. The catalog alone runs to
	// tens of kilobytes. Rendering them would bury a two-line exchange under
	// pages of machine text on a phone screen.
	//
	// The check is `!= ""` rather than `== "user"` so that a log written before
	// the source field existed still renders instead of silently emptying.
	if ev.Data.Source.Kind != "" && ev.Data.Source.Kind != "user" {
		return
	}

	text := joinText(ev.Data.Content)
	files := attachments(ev.Data.Content)
	if strings.TrimSpace(text) == "" && files == 0 {
		// An injection with no body at all. Rendering an empty bubble would be
		// noise.
		return
	}
	if p.meta.Preview == "" {
		// A prompt that was only a screenshot still needs a list row, and "2
		// attachments" is a truer one than a blank title.
		if strings.TrimSpace(text) == "" {
			p.meta.Preview = attachmentLabel(files)
		} else {
			p.meta.Preview = preview(text)
		}
	}
	id := ev.Data.ID
	if id == "" {
		id = fmt.Sprintf("u-%d", env.Seq)
	}
	p.append(Item{
		ID:          id,
		Seq:         env.Seq,
		Time:        fromMillis(env.Time),
		Role:        RoleUser,
		Text:        text,
		Attachments: files,
	})
}

// attachmentLabel is how a message made only of attachments is named, for a list
// row that has no words to show. The app renders its own wording from
// Item.Attachments when it has one; this is the fallback, and it lives here so
// that a session whose first prompt was a screenshot is recognisable in every
// client rather than only the one that was updated.
func attachmentLabel(n int) string {
	if n == 1 {
		return "Image"
	}
	return fmt.Sprintf("%d images", n)
}

func (p *parser) feedAssistantMessage(line []byte, env envelope) {
	var ev struct {
		Data struct {
			Message struct {
				Content []contentBlock `json:"content"`
				Source  struct {
					Model string `json:"model"`
				} `json:"source"`
				ID string `json:"id"`
			} `json:"message"`
			Usage *struct {
				InputTokens     int `json:"inputTokens"`
				OutputTokens    int `json:"outputTokens"`
				TotalTokens     int `json:"totalTokens"`
				CacheReadTokens int `json:"cacheReadTokens"`
			} `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("assistant message", err)
		return
	}

	// Text and reasoning are separated so the UI can collapse the reasoning.
	// `tool-call` blocks are skipped on purpose: the `tool/call` and
	// `tool/result` events describe the same invocations with more detail
	// (arguments as text, plus the result), and rendering both would duplicate
	// every tool card.
	var text, thinking strings.Builder
	for _, b := range ev.Data.Message.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "reasoning":
			if thinking.Len() > 0 {
				thinking.WriteString("\n\n")
			}
			thinking.WriteString(b.Text)
		}
	}

	if strings.TrimSpace(text.String()) == "" && strings.TrimSpace(thinking.String()) == "" {
		// A message that is nothing but tool calls. Those become their own items.
		return
	}

	item := Item{
		ID:       ev.Data.Message.ID,
		Seq:      env.Seq,
		Time:     fromMillis(env.Time),
		Role:     RoleAssistant,
		Text:     text.String(),
		Thinking: thinking.String(),
		Model:    ev.Data.Message.Source.Model,
	}
	if item.ID == "" {
		item.ID = fmt.Sprintf("a-%d", env.Seq)
	}
	if u := ev.Data.Usage; u != nil {
		item.Usage = &Usage{
			InputTokens:     u.InputTokens,
			OutputTokens:    u.OutputTokens,
			TotalTokens:     u.TotalTokens,
			CacheReadTokens: u.CacheReadTokens,
		}
	}
	p.append(item)
}

func (p *parser) feedToolCall(line []byte, env envelope) {
	var ev struct {
		Data struct {
			CallID    string `json:"callId"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("tool call", err)
		return
	}
	if ev.Data.CallID == "" {
		return
	}

	if !p.keepItems {
		return
	}
	p.toolIndex[ev.Data.CallID] = len(p.items)
	p.append(Item{
		ID:      ev.Data.CallID,
		Seq:     env.Seq,
		Time:    fromMillis(env.Time),
		Role:    RoleTool,
		Tool:    ev.Data.Name,
		Input:   ev.Data.Arguments,
		Pending: true,
	})
}

func (p *parser) feedToolResult(line []byte, env envelope) {
	var ev struct {
		Data struct {
			Message struct {
				ToolCallID string         `json:"toolCallId"`
				Content    []contentBlock `json:"content"`
				IsError    bool           `json:"isError"`
				ID         string         `json:"id"`
			} `json:"message"`
			// Error is DSH's structured failure, recorded beside the message:
			// {"name":"FsError","code":"FS_NOT_OBSERVED"}. It survives the
			// projection because a code is what a reader can act on, while the
			// message text is prose.
			Error struct {
				Name string `json:"name"`
				Code string `json:"code"`
			} `json:"error"`
		} `json:"data"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		p.debug("tool result", err)
		return
	}

	if !p.keepItems {
		return
	}
	output := joinText(ev.Data.Message.Content)
	ended := fromMillis(env.Time)
	facts := toolresult.Observe(output).WithError(ev.Data.Error.Name, ev.Data.Error.Code).Facts()

	callID := ev.Data.Message.ToolCallID
	idx, ok := p.toolIndex[callID]
	if !ok {
		// A result with no matching call: the log was truncated, or the call
		// predates the retained prefix. Emit a standalone item rather than
		// dropping the output.
		p.append(Item{
			ID:      ev.Data.Message.ID,
			Seq:     env.Seq,
			Time:    fromMillis(env.Time),
			Role:    RoleTool,
			Tool:    "tool",
			Output:  output,
			IsError: ev.Data.Message.IsError,
			EndedAt: &ended,
			Facts:   facts,
		})
		return
	}

	it := &p.items[idx]
	it.Output = output
	it.IsError = ev.Data.Message.IsError
	it.Pending = false
	it.EndedAt = &ended
	it.Facts = facts
	// The result's sequence orders the completion, but the item keeps the call's
	// position so the transcript reads call-then-result.
	delete(p.toolIndex, callID)
}

// append records an item, or only counts it when the caller wants metadata alone.
func (p *parser) append(it Item) {
	if it.Role == RoleUser || it.Role == RoleAssistant {
		p.meta.Messages++
	}
	if !p.keepItems {
		return
	}
	p.items = append(p.items, it)
}

// preview turns a first message into one line a list row can show.
func preview(text string) string {
	line := strings.TrimSpace(text)
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = strings.TrimSpace(line[:index])
	}
	const limit = 140
	if len(line) > limit {
		// Trim on a rune boundary so a multi-byte character is never cut in half.
		cut := limit
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = strings.TrimSpace(line[:cut]) + "…"
	}
	return line
}

// finish returns the folded transcript, or the header's refusal.
func (p *parser) finish() ([]Item, Meta, error) {
	if p.headerErr != nil {
		return nil, Meta{}, p.headerErr
	}
	return p.items, p.meta, nil
}

func (p *parser) debug(msg string, err error) {
	if p.logger != nil {
		p.logger.Debug("sessionlog: "+msg, "session", p.sessionID, "error", err.Error())
	}
}

// joinText concatenates the textual parts of a content array.
func joinText(blocks []contentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// fromMillis converts a JavaScript epoch-millisecond timestamp.
func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

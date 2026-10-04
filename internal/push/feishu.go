package push

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// This file renders one Message as a Feishu interactive card.
//
// It is deliberately separate from webhook.go, which knows only how to post JSON
// to a chat service. A card is a *presentation* decision — what a reader sees
// first, how much of an answer one message may carry, which markup a provider
// interprets — and a second provider means a second file beside this one
// (payload in webhook.go routes to it), not a second branch inside the transport.
//
// The card answers two questions in the order they are asked — *which
// conversation*, and *what did it say* — and everything else is arranged around
// them:
//
//	┌──────────────────────────────────────────┐
//	│ <the session's own name>                 │  header title, the largest text
//	├──────────────────────────────────────────┤
//	│ ✅ Completed · Main agent · 🕐 9m 37s …  │  status line, small and grey
//	│ ──────────────────────────────────────── │
//	│ <the model's closing message>            │  the body: the answer itself
//	│ ──────────────────────────────────────── │
//	│ [ Open the session ]                     │  the one action
//	└──────────────────────────────────────────┘
//
// The title is the session's name and nothing else. It used to be a sentence —
// "completed · Main agent · <name>" — which spent the largest text the client
// draws on two facts the reader already knew and left the name wrapped behind
// them; the outcome and the actor are now the status line, where they are
// scanned rather than read. What the message *means* is decided by the notifier
// (see Message); this file only decides what a chat client is shown.

// rendered is one message, decided once and laid out per dialect.
//
// The two card formats below differ only in how they say these seven things —
// what the title is, which colour the header takes, what the status line says,
// what the body is, whether the body was cut, where the button goes and what it
// is called. Deciding them once means a rule cannot be implemented in the rich
// card and forgotten in the plain one.
type rendered struct {
	title     string
	template  string
	meta      string
	body      string
	truncated bool
	link      string
	button    string
}

// render decides what one message shows, in the form both dialects share.
func (w *Webhook) render(message Message, format cardFormat) rendered {
	body, truncated := w.cardBody(message, format)
	return rendered{
		title:     headline(message),
		template:  feishuTemplate(message),
		meta:      metaLine(message),
		body:      body,
		truncated: truncated,
		link:      w.absolute(message.URL),
		button:    buttonLabel(message, truncated),
	}
}

// feishuPayload builds a card in the current format (JSON 2.0).
//
// This is the one that renders markdown: headings, lists, tables and — the
// reason it matters most for this product — code blocks. A card 1.0 text element
// is lark_md, which renders bold, links and mentions and treats `##`, `|` and
// ``` as literal characters, so an agent's answer arrives as punctuation soup.
// See feishuPlainPayload for what a service that refuses this format is given
// instead.
//
// What is known about the format, from cards posted to a live tenant rather than
// from documentation — the properties below were chosen by that evidence, and a
// property this format does not know is not a cosmetic failure: the whole card is
// refused and the channel downgrades, giving up markdown entirely.
//
//   - `markdown` renders headings, bold, italic, strikethrough, inline code,
//     links, ordered/unordered/nested lists, bordered tables, and code blocks
//     with syntax highlighting and line numbers.
//   - A table wider than the card is clipped at its right edge, which is the
//     platform's business, not something a sender can fix.
//   - `text_size: "notation"` is footnote-sized grey and wraps. A header
//     `subtitle` is not a substitute: it takes the header's own colour, stays on
//     one line, and truncates — a long status line came back as "🕐 9m…".
//
// The layout is:
//
//	┌──────────────────────────────────────────┐
//	│ <the session's own name>                 │  header title
//	├──────────────────────────────────────────┤
//	│ ✅ Completed · Main agent · 🕐 9m 37s …  │  status line
//	│ ──────────────────────────────────────── │
//	│ <the model's closing message>            │  markdown, rendered
//	│ ──────────────────────────────────────── │
//	│ [ Open the session ]                     │  the one action
//	└──────────────────────────────────────────┘
func (w *Webhook) feishuPayload(message Message) ([]byte, error) {
	parts := w.render(message, cardRich)

	elements := make([]any, 0, 4)
	if parts.meta != "" {
		// notation, so that the status is a footnote to the answer rather than a
		// line of it — the one thing the plain format did better, by accident of
		// having a `note` element.
		elements = append(elements, map[string]any{
			"tag":       "markdown",
			"content":   parts.meta,
			"text_size": "notation",
		})
	}
	if parts.body != "" {
		if len(elements) > 0 {
			// The rule between what the card is *about* and what the gateway
			// knows *about* it. Only drawn when there is something above it, so
			// a card that is nothing but an answer has no stray line.
			elements = append(elements, map[string]any{"tag": "hr"})
		}
		elements = append(elements, map[string]any{"tag": "markdown", "content": parts.body})
	}
	if parts.link != "" {
		elements = append(elements, map[string]any{
			"tag":   "button",
			"text":  map[string]any{"tag": "plain_text", "content": parts.button},
			"type":  "primary",
			"width": "fill",
			// 2.0 moved the link out of the button and into a behaviour, so that
			// one control can do more than open a URL.
			"behaviors": []any{map[string]any{"type": "open_url", "default_url": parts.link}},
		})
	}

	card := map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": parts.title},
			"template": parts.template,
		},
		"body": map[string]any{"direction": "vertical", "elements": elements},
	}

	return json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
}

// feishuPlainPayload builds the same card in the first-generation format.
//
// It is what a chat service that refuses the 2.0 shape is given, and it is not a
// degraded copy of the rich card: the answer is folded into what lark_md can
// actually render (see flattenMarkdown), so the reader gets the agent's words
// with structure rewritten rather than a page of markdown punctuation.
func (w *Webhook) feishuPlainPayload(message Message) ([]byte, error) {
	parts := w.render(message, cardPlain)

	elements := make([]any, 0, 4)
	if parts.meta != "" {
		// plain_text rather than lark_md: this line is written here, so it has no
		// markdown to interpret, and the simpler tag is the one every client
		// renders identically.
		elements = append(elements, map[string]any{
			"tag":      "note",
			"elements": []any{map[string]any{"tag": "plain_text", "content": parts.meta}},
		})
	}
	if parts.body != "" {
		if len(elements) > 0 {
			elements = append(elements, map[string]any{"tag": "hr"})
		}
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": parts.body},
		})
	}
	if parts.link != "" {
		elements = append(elements, map[string]any{
			"tag": "action",
			"actions": []any{map[string]any{
				"tag":  "button",
				"text": map[string]any{"tag": "plain_text", "content": parts.button},
				"type": "primary",
				"url":  parts.link,
			}},
		})
	}

	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": parts.title},
			"template": parts.template,
		},
		"elements": elements,
	}

	return json.Marshal(map[string]any{"msg_type": "interactive", "card": card})
}

// headline is the card's title: the name of the thing it is about.
//
// A message with no title at all still has to say something, and the product's
// own name is the honest answer — it is what a test notification is, and it
// reads better than an empty header.
func headline(message Message) string {
	if title := strings.TrimSpace(message.Title); title != "" {
		return title
	}
	return "dsh-gateway"
}

// feishuTemplate is the header colour, which is the only urgency a chat client
// lets us express without a word.
//
// Red is the two outcomes that cost something: an approval that expired and was
// refused, and a turn that failed and left no result behind. Green is the one
// that worked — a card that reported success in the same blue as everything else
// made the reader look for the word. The approval tag is checked as well as the
// outcome because a card built by hand (the settings screen's test button, a
// caller that predates the outcome vocabulary) may carry neither.
func feishuTemplate(message Message) string {
	switch {
	case message.Outcome == "failed" || message.Outcome == "expired":
		return "red"
	case message.Outcome == "waiting" || strings.HasPrefix(message.Tag, "approval-"):
		return "orange"
	case message.Outcome == "completed":
		return "green"
	case message.Outcome == "cancelled":
		return "grey"
	default:
		return "blue"
	}
}

// metaLine is the small grey line under the title: everything the notification
// knows *about* the work, in the order a reader checks it.
//
//	✅ Completed · Main agent · 🕐 9m 37s · 5 tool calls · deepseek-v4.1-flash
//	⏳ Waiting for you · Main agent
//
// The status leads because it decides whether the rest matters. The summary is
// skipped when the title already contains it — an approval card is titled
// "Approval needed · bash" and would otherwise repeat "bash" directly beneath
// itself, which is the duplication this whole change exists to remove. The test
// is a substring rather than a field because the duplication is a property of
// what the reader sees, and the worst case is a statistic that goes unsaid.
func metaLine(message Message) string {
	parts := make([]string, 0, 5)
	if status := outcomeStatus(message.Outcome); status != "" {
		if message.Actor != nil {
			status += " · " + message.Actor.Label()
		}
		parts = append(parts, status)
	}
	if duration := humanDuration(time.Duration(message.DurationMS) * time.Millisecond); duration != "" {
		parts = append(parts, "🕐 "+duration)
	}
	if summary := strings.TrimSpace(message.Summary); summary != "" && !strings.Contains(message.Title, summary) {
		parts = append(parts, summary)
	}
	if model := strings.TrimSpace(message.Model); model != "" {
		parts = append(parts, model)
	}
	return strings.Join(parts, " · ")
}

// outcomeStatus is a settled outcome as a card's status line prints it.
//
// It is the same vocabulary outcomeWord produces — the wire's own word, so a
// reader who sees "completed" in the app and "Completed" in the card is reading
// one system rather than two — with a mark in front of it, because a chat client
// draws a card's first line in the same weight whatever it says, and the mark is
// what makes it scannable rather than read. Empty for an outcome this build does
// not know, so a message with no outcome (a test notification) grows no status
// line at all.
//
// It lives here rather than beside outcomeWord because it is card vocabulary: the
// emoji is an affordance of a small grey line, and the app has its own.
func outcomeStatus(outcome string) string {
	switch outcome {
	case "completed":
		return "✅ Completed"
	case "failed":
		return "❌ Failed"
	case "cancelled":
		return "⏹ Stopped"
	case "waiting":
		return "⏳ Waiting for you"
	case "expired":
		return "⌛ Expired"
	default:
		return ""
	}
}

// humanDuration renders how long a turn ran, at the resolution a reader cares
// about.
//
// Seconds for a short turn, because that is the unit the reader is judging ("a
// five-second answer is not worth a notification" is a thought in seconds);
// minutes and seconds for the long ones, which is where the threshold this
// product sets actually lives. Zero is empty rather than "0s": nobody measured
// it, and a zero that reads as a measurement is worse than a gap.
func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return itoa(int(d.Round(time.Second)/time.Second)) + "s"
	case d < time.Hour:
		minutes := int(d / time.Minute)
		seconds := int(d.Round(time.Second)/time.Second) - minutes*60
		if seconds == 0 {
			return itoa(minutes) + "m"
		}
		return itoa(minutes) + "m " + itoa(seconds) + "s"
	default:
		hours := int(d / time.Hour)
		minutes := int(d/time.Minute) - hours*60
		if minutes == 0 {
			return itoa(hours) + "h"
		}
		return itoa(hours) + "h " + itoa(minutes) + "m"
	}
}

// cardBody is what the card is about, and whether it had to be cut.
//
// The answer when this channel is allowed to carry it and the turn produced one;
// otherwise the message's own body, which is the lock screen's line — a count of
// the work, or the harness's failure detail. That fallback is what keeps a
// channel with answers turned off behaving exactly as it did before, rather than
// leaving a hole where the product used to say something.
//
// Two transforms wrap the budget, and both are per dialect. Defusing a mention
// comes first, because it changes text the reader will see and a budget applied
// to text that is then rewritten is not the budget they get. Folding markdown
// into the plain dialect comes before the budget too, for the same reason and
// with one more: the fold is what makes the answer shorter or longer, so
// measuring first would cut a card that had room to spare.
func (w *Webhook) cardBody(message Message, format cardFormat) (string, bool) {
	if w.IncludeAnswer {
		if answer := strings.TrimSpace(message.Answer); answer != "" {
			answer = neutraliseMention(answer)
			if format == cardPlain {
				answer = flattenMarkdown(answer)
			}
			return answerText(answer, w.answerBudget())
		}
	}
	return strings.TrimSpace(message.Body), false
}

// answerBudget is how much of an answer this channel prints.
func (w *Webhook) answerBudget() int {
	if w.MaxAnswerChars > 0 {
		return w.MaxAnswerChars
	}
	return DefaultAnswerChars
}

// buttonLabel is the call to action, said in terms of what the reader will find
// on the other side of it.
//
// "Open the session" is right when the card already showed the answer; when it
// had to cut one, the button is where the rest is; when the turn failed, the
// thing worth opening for is the error rather than a result.
func buttonLabel(message Message, truncated bool) string {
	switch message.Outcome {
	case "failed":
		return "Open the session for the error"
	case "waiting":
		return "Open the session to decide"
	}
	if truncated {
		return "Open the session for the rest"
	}
	return "Open the session"
}

// mentionMarkup matches the one lark_md construct with an effect beyond the look
// of the card.
var mentionMarkup = regexp.MustCompile(`(?i)<at[\s>]`)

// neutraliseMention defuses a mention inside text the gateway did not write.
//
// `<at id=all></at>` in a card mentions the group. The text here comes from a
// model that has been reading files and tool output, so a repository containing
// that string is a way to make the operator's bot ping five hundred people — and
// the cost of the guard is one visible character in the rare answer that
// contains such a tag without meaning it. The other tags lark_md knows (font,
// links) only change how the model's own words are drawn, which is a rendering
// decision, not an action taken on the reader's behalf.
func neutraliseMention(text string) string {
	if !strings.Contains(text, "<") {
		return text
	}
	return mentionMarkup.ReplaceAllStringFunc(text, func(match string) string {
		return "‹" + match[1:]
	})
}

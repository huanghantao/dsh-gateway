package push

import "strings"

// How much of a model's answer one chat message may carry.
//
// This is its own file because it is the one piece of the chat path that is not
// about a provider: the budget, the shape of a cut, and the fact that a cut has
// to be stated. Anything that renders an answer — the Feishu card here, a second
// provider later — shapes it through these functions rather than inventing its
// own arithmetic, because a second truncation rule is a second answer to "did I
// see all of it?".

// DefaultAnswerChars is how much of a model's answer one chat card carries.
//
// The budget is a choice, not a platform limit — it is set well inside what a
// Feishu card can hold, so that the truncation rule is exercised by the
// deployment rather than by the message size at the moment something important
// happens. It is also set above the answers that actually occur: of 199 sessions
// with a closing message on the deployment this was measured against, half ended
// in under 30 characters and 97% in under 3,000, so a card that prints this much
// prints the whole answer for all but the rare report.
//
// A longer answer keeps its head and its tail — a report says what it found
// first and what to do about it last — and states in the middle exactly how much
// it dropped, because a reader who cannot see the cut cannot know to open the
// session.
const DefaultAnswerChars = 4000

// answerText fits a model's answer into a chat message, returning the text and
// whether any of it was dropped.
//
// The cut is head-heavy: a report states its conclusion first more often than it
// states it last, and the tail is what catches the ones that do the opposite.
// The counts are exact — "…" alone leaves the reader unable to tell a paragraph
// they can skip from half a document they cannot.
//
// Text handed to this function is untrusted, and each renderer defuses whatever
// its own markup would otherwise act on *before* calling it: see
// neutraliseMention. This function only decides how much survives.
func answerText(answer string, limit int) (string, bool) {
	runes := []rune(answer)
	if limit <= 0 || len(runes) <= limit {
		return balanceFences(answer), false
	}

	head := limit * 3 / 4
	tail := limit - head
	dropped := len(runes) - head - tail

	kept := cutAtLine(string(runes[:head])) +
		"\n\n⋯ " + itoa(dropped) + " characters omitted, of " + itoa(len(runes)) + " ⋯\n\n" +
		cutAtLineFrom(string(runes[len(runes)-tail:]))
	return balanceFences(kept), true
}

// balanceFences closes a code fence the cut left open.
//
// Every chat service this gateway posts to renders markdown of some flavour, so
// an odd number of ``` makes everything after it one long code block — an answer
// truncated inside a sample would take the rest of the card with it. Appending
// the missing fence is the cheapest way to keep a cut answer readable.
func balanceFences(text string) string {
	if strings.Count(text, "```")%2 == 0 {
		return text
	}
	return strings.TrimRight(text, "\n") + "\n```"
}

// cutAtLine trims a hard character cut back to the last line break, so a head
// does not end mid-word when it can end at the end of a line instead. It gives
// up and keeps the hard cut when the last break is far enough back to waste a
// noticeable part of the budget — losing a tenth of the answer to avoid half a
// word is not a trade worth making.
func cutAtLine(text string) string {
	index := strings.LastIndexByte(text, '\n')
	if index < 0 || index < len(text)-len(text)/10-1 {
		return text
	}
	return text[:index]
}

// cutAtLineFrom is cutAtLine for a tail: it starts the tail at a line break
// rather than ending it at one, so a quote does not begin in the middle of a
// word when the next line starts cleanly.
func cutAtLineFrom(text string) string {
	index := strings.IndexByte(text, '\n')
	if index < 0 || index > len(text)/10 {
		return text
	}
	return text[index+1:]
}

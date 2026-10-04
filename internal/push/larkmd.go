package push

import "strings"

// Folding a markdown document into what a first-generation card can render.
//
// A Feishu card 1.0 text element is `lark_md`, which is a *subset*: bold, italic,
// strikethrough, links and mentions. It has no headings, no tables, no code
// blocks, no rules and no lists — a `##` or a `|` is a literal character, and an
// agent's answer full of them arrives as punctuation soup. Cards 2.0 render real
// markdown (see feishu.go), and that is what a deployment should get; this file
// is what the ones that cannot are given instead.
//
// The transform is deliberately lossy in one direction only: structure that
// lark_md cannot express is rewritten into something it can (a heading becomes a
// bold line, a table row becomes a ` · `-joined line, a rule becomes a blank
// line), and inline markup that it *does* render is left exactly as it was. It
// never invents emphasis and never reflows prose, because the reader is comparing
// this against what the agent actually said.

// flattenMarkdown rewrites a markdown document into the subset lark_md renders.
func flattenMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	// tableRows counts the rows of the table being read, so that its first row
	// can be emphasised as a header without inspecting what was already printed.
	tableRows := 0

	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)

		// Fenced code: the markers go, the code stays. There is no monospace in
		// lark_md, so the alternative is dropping the sample entirely — and for
		// an agent whose answer often *is* a command or a diff, that would drop
		// the answer.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}

		// Past the fence check the line is prose of some kind — a paragraph, a
		// heading, a list item, a table row, a quote — and inline code markup is
		// punctuation in all of them. Folding it here rather than in the default
		// branch is what keeps a `` `##` `` inside a table cell from surviving as
		// backticks.
		line = stripInlineCode(line)
		trimmed = strings.TrimSpace(line)

		// Anything that is not a table row ends the table being read.
		if !strings.HasPrefix(trimmed, "|") {
			tableRows = 0
		}

		// A rule is a blank line: the card separates its own sections with real
		// dividers, and a line of dashes in the middle of prose reads as noise.
		if isRule(trimmed) {
			out = append(out, "")
			continue
		}

		if heading, ok := headingText(trimmed); ok {
			out = append(out, "**"+heading+"**")
			continue
		}

		// A table becomes its rows, one per line, cells joined by a separator. It
		// loses column alignment — nothing in lark_md can keep it — but it keeps
		// the pairing of cell to cell, which is what a table was for.
		if strings.HasPrefix(trimmed, "|") && strings.HasSuffix(trimmed, "|") {
			if isTableRule(trimmed) {
				continue
			}
			row := strings.Join(tableCells(trimmed), " · ")
			if tableRows == 0 {
				// The first row of a table is its header, and a header is worth
				// the same emphasis a heading gets.
				row = "**" + row + "**"
			}
			tableRows++
			out = append(out, row)
			continue
		}

		// A list marker becomes a bullet character, because lark_md renders `- `
		// as a hyphen and a reader counting items deserves better than that.
		if marker, rest, ok := listItem(line); ok {
			out = append(out, marker+" "+rest)
			continue
		}

		// A quote keeps its text and loses its marker; indentation is not
		// something either card format preserves, so the two-space form at least
		// reads as a quotation.
		if strings.HasPrefix(trimmed, "> ") {
			out = append(out, "  "+strings.TrimPrefix(trimmed, "> "))
			continue
		}

		out = append(out, line)
	}

	return strings.TrimRight(strings.Join(out, "\n"), " \n\t")
}

// stripInlineCode removes the backticks around inline code.
//
// lark_md has no inline code either, so it draws them: a sentence about
// `webhook.go` arrives with the backticks in it, which is punctuation the reader
// has to filter out of prose. The word survives; only the markup goes.
func stripInlineCode(line string) string {
	if !strings.Contains(line, "`") {
		return line
	}
	var out strings.Builder
	out.Grow(len(line))
	run := 0
	flush := func() {
		if run > 2 {
			// Three or more is a fence this line should not have had.
			out.WriteString(strings.Repeat("`", run))
		}
		run = 0
	}
	for _, r := range line {
		if r == '`' {
			run++
			continue
		}
		flush()
		out.WriteRune(r)
	}
	flush()
	return out.String()
}

// isRule reports whether a line is a thematic break.
func isRule(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	switch trimmed[0] {
	case '-', '*', '_':
	default:
		return false
	}
	for _, r := range trimmed {
		if r != rune(trimmed[0]) {
			return false
		}
	}
	return true
}

// headingText strips an ATX heading marker, and reports whether there was one.
func headingText(trimmed string) (string, bool) {
	hashes := 0
	for hashes < len(trimmed) && trimmed[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || hashes > 6 || hashes == len(trimmed) || trimmed[hashes] != ' ' {
		return "", false
	}
	return strings.TrimSpace(strings.TrimRight(trimmed[hashes:], "#")), true
}

// isTableRule reports whether a table line is its `|---|---|` separator.
func isTableRule(trimmed string) bool {
	for _, r := range trimmed {
		switch r {
		case '|', '-', ':', ' ':
		default:
			return false
		}
	}
	return true
}

// tableCells splits a table row into its cells.
func tableCells(trimmed string) []string {
	trimmed = strings.Trim(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}

// listItem reads a bullet or ordered-list marker, returning the bullet to print
// and the item's own text. Indentation is preserved so a nested list still reads
// as nested.
func listItem(line string) (string, string, bool) {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimLeft(line, " \t")
	if body == "" {
		return "", "", false
	}
	if marker := body[0]; marker == '-' || marker == '*' || marker == '+' {
		if len(body) > 1 && body[1] == ' ' {
			return indent + "•", strings.TrimSpace(body[2:]), true
		}
		return "", "", false
	}
	// "1. item", "2) item": the number is the reader's, so it is kept.
	digits := 0
	for digits < len(body) && body[digits] >= '0' && body[digits] <= '9' {
		digits++
	}
	if digits > 0 && digits+1 < len(body) && (body[digits] == '.' || body[digits] == ')') && body[digits+1] == ' ' {
		return indent + body[:digits+1], strings.TrimSpace(body[digits+2:]), true
	}
	return "", "", false
}

package push

import "testing"

// TestFlattenMarkdown pins the fold, construct by construct.
//
// Every case here is a construct an agent's answer actually contains, and the
// expectation is what a reader sees in a card whose text element is lark_md:
// something rather than punctuation. The two rules the cases encode are that
// structure lark_md cannot draw is rewritten into something it can, and that
// markup it *does* draw — bold, links — is passed through untouched.
func TestFlattenMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "headings become bold lines",
			in:   "## 一、分层错误\n### ① 最严重",
			want: "**一、分层错误**\n**① 最严重**",
		},
		{
			name: "a rule becomes a blank line",
			in:   "上文\n\n---\n\n下文",
			want: "上文\n\n\n\n下文",
		},
		{
			name: "bullets become bullet characters",
			in:   "- 第一条\n- 第二条",
			want: "• 第一条\n• 第二条",
		},
		{
			name: "a nested list keeps its nesting",
			in:   "- 外层\n  - 内层",
			want: "• 外层\n  • 内层",
		},
		{
			name: "an ordered list keeps its numbers",
			in:   "1. 第一步\n2. 第二步",
			want: "1. 第一步\n2. 第二步",
		},
		{
			name: "a code block keeps its code and loses its markers",
			in:   "跑一下：\n\n```go\ngo test ./...\n```\n",
			want: "跑一下：\n\ngo test ./...",
		},
		{
			name: "a table keeps its cells, row by row",
			in:   "| 文件 | 行数 |\n|---|---|\n| webhook.go | 171 |\n| feishu.go | 286 |",
			want: "**文件 · 行数**\nwebhook.go · 171\nfeishu.go · 286",
		},
		{
			name: "a quote keeps its words",
			in:   "> 这行是引用",
			want: "  这行是引用",
		},
		{
			name: "inline code loses its backticks",
			in:   "改之前 `webhook.go` 有 451 行。",
			want: "改之前 webhook.go 有 451 行。",
		},
		{
			name: "inline code inside a table cell loses its backticks too",
			in:   "| 元素 | 1.0 卡片 |\n|---|---|\n| 标题 `##` | 字面量 |",
			want: "**元素 · 1.0 卡片**\n标题 ## · 字面量",
		},
		{
			name: "inline code inside a quote loses its backticks too",
			in:   "> 引用：`flattenMarkdown` 负责压平。",
			want: "  引用：flattenMarkdown 负责压平。",
		},
		{
			name: "emphasis and links are left alone",
			in:   "**粗体**、*斜体* 和 [链接](https://example.test)。",
			want: "**粗体**、*斜体* 和 [链接](https://example.test)。",
		},
		{
			name: "an asterisk bullet is not mistaken for emphasis",
			in:   "* 一条",
			want: "• 一条",
		},
		{
			name: "trailing blank lines go",
			in:   "正文\n\n\n",
			want: "正文",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flattenMarkdown(tc.in); got != tc.want {
				t.Errorf("flattenMarkdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFlattenMarkdownIsStable: folding text that is already plain must not change
// it, or a second-generation card's body would drift every time a channel
// downgraded and came back.
func TestFlattenMarkdownIsStable(t *testing.T) {
	plain := "一行普通的回答，没有标记。\n第二行也是。"
	if got := flattenMarkdown(plain); got != plain {
		t.Errorf("plain text was rewritten: %q", got)
	}
}

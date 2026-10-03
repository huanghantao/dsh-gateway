package v1

import (
	"github.com/huanghantao/dsh-gateway/internal/config"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/sessionlog"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// configView renders harness configuration options for the wire.
//
// It exists here rather than being shared with the events bridge because the two
// serve different readers: the bridge shapes an event payload, this shapes a
// response body. Keeping them separate means a change to one cannot silently
// alter the other's contract.
func configView(options []harness.ConfigOption) []map[string]any {
	out := make([]map[string]any, 0, len(options))
	for _, o := range options {
		values := make([]map[string]any, 0, len(o.Options))
		for _, v := range o.Options {
			values = append(values, map[string]any{
				"id":          v.ID,
				"name":        v.Name,
				"description": v.Description,
				"group":       v.Group,
				"label":       v.Label(),
			})
		}
		out = append(out, map[string]any{
			"id":      o.ID,
			"name":    o.Name,
			"current": o.Current,
			"options": values,
		})
	}
	return out
}

// transcriptPageView is one page of history, with its tool payloads bounded.
type transcriptPageView struct {
	Items      []transcriptItemView `json:"items"`
	NextBefore int64                `json:"nextBefore,omitempty"`
	Total      int                  `json:"total"`
	// Unsupported tells the client the log was written by a newer DSH than this
	// build understands, so the items are empty rather than wrong.
	Unsupported bool `json:"unsupported,omitempty"`
}

// transcriptItemView is one history row.
//
// It embeds the projection and shadows the two fields a byte budget applies to.
// The shadowing is deliberate: the outer field wins in encoding/json, so every
// other field of a sessionlog.Item keeps reaching the client unchanged as the
// projection grows, while the two that are bounded cannot be forgotten.
//
// Copying matters as much as bounding. A page comes from the store's cache and
// the change projection folds the *full* argument text to build its diffs;
// bounding in place would silently truncate a diff for every later reader.
type transcriptItemView struct {
	sessionlog.Item
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
	toolresult.Trim
}

// transcriptView bounds every tool payload in a page.
func transcriptView(page sessionlog.Page, limits config.Limits) transcriptPageView {
	items := make([]transcriptItemView, 0, len(page.Items))
	for _, item := range page.Items {
		view := transcriptItemView{Item: item}
		if item.Role == sessionlog.RoleTool {
			view.Input, view.Output, view.Trim = toolresult.Bound(
				item.Input, item.Output, limits.ToolInputBytes, limits.ToolOutputBytes)
		}
		items = append(items, view)
	}
	return transcriptPageView{Items: items, NextBefore: page.NextBefore, Total: page.Total}
}

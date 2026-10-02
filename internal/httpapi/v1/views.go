package v1

import "github.com/huanghantao/dsh-gateway/internal/harness"

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

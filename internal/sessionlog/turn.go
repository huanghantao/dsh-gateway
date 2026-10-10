package sessionlog

import (
	"strings"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// LastTurn states what a session's most recent turn was, read off a projection
// of its log.
//
// A followed turn has no update stream to count — the gateway did not run it —
// so what this process happened to watch arrive is not the turn, it is the part
// of the turn that overlapped with this process being alive. The log has all of
// it: every row the turn wrote, from its `turn/start` boundary to the `turn/end`
// that settled it. Stating the record from that is what makes a notification
// about a followed turn describe the turn rather than the observer.
//
// startedAt is the turn's own start (Meta.TurnStartedAt) and is what separates
// this turn's rows from every earlier turn's. A zero value means the log does
// not say, and the whole projection is read as one turn — the honest fallback
// for a log whose boundaries carry no timestamps. A row with no timestamp of its
// own is read the same way: it belongs to the turn being described, because
// nothing says otherwise.
func LastTurn(items []Item, startedAt time.Time) events.TurnRecord {
	var record events.TurnRecord
	for _, item := range items {
		if !inTurn(item, startedAt) {
			continue
		}
		switch item.Role {
		case RoleAssistant:
			// The last thing the turn said, not the first, and never an empty
			// step: a step that only called tools commits a message with no
			// text, and treating that as the turn's sign-off would erase the
			// answer a later step had already written.
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			record.Closing = &events.Closing{Text: item.Text, Model: item.Model}

		case RoleTool:
			// A call still in flight is not work the turn did: its result never
			// arrived, so nothing can say how it ended.
			if item.Pending {
				continue
			}
			record.Work.Count(item.Tool, toolresult.Failed(item.IsError, item.Facts))

		case RoleUser, RoleNotice:
			// Neither is the turn's own: a prompt is what the operator typed,
			// and a notice is the harness annotating the conversation — a
			// delegated child's settlement, most of them. Only an assistant row
			// can be the closing message, and only a tool row is work, which is
			// why the two are the whole of what is read here.
		}
	}
	return record
}

// inTurn reports whether a row was written by the turn that began at startedAt.
func inTurn(item Item, startedAt time.Time) bool {
	if startedAt.IsZero() || item.Time.IsZero() {
		return true
	}
	return !item.Time.Before(startedAt)
}

package v1

import (
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/harness"
)

// sessionListTTL is how long a listing from the harness is reused.
//
// The harness enumerates every session it knows about, which costs it a walk of
// the session store: measured at 140–260ms on a machine with a few hundred
// sessions, and by far the most expensive thing in a list request now that the
// gateway's own per-session work is cached. A phone pulls to refresh; it does
// not need a second enumeration a moment later, and a session that appears in
// between (one the operator just created) invalidates the entry directly.
const sessionListTTL = 2 * time.Second

// sessionListCache holds the last harness listing per (workspace, cursor).
//
// Deliberately tiny: one page, one key. The phone asks for the first page over
// and over, and that is the request worth making cheap.
type sessionListCache struct {
	mu        sync.Mutex
	at        time.Time
	workspace string
	cursor    string
	page      harness.SessionPage
	valid     bool
}

// get returns a cached page when it is fresh and for the same request.
func (c *sessionListCache) get(workspace, cursor string, now time.Time) (harness.SessionPage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.valid || c.workspace != workspace || c.cursor != cursor {
		return harness.SessionPage{}, false
	}
	if now.Sub(c.at) > sessionListTTL {
		return harness.SessionPage{}, false
	}
	return c.page, true
}

// put records a page.
func (c *sessionListCache) put(workspace, cursor string, page harness.SessionPage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workspace, c.cursor, c.page, c.at, c.valid = workspace, cursor, page, now, true
}

// invalidate drops the entry, so the next request asks the harness again.
func (c *sessionListCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.valid = false
}

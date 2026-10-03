package approvals

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"time"
)

// The two options this gateway synthesises, on top of the allow-once and
// reject-once the harness offers.
//
// They exist because consent is being collected at the wrong granularity
// otherwise. "This session may run read-only tools for the next half hour" is a
// decision a person can genuinely make once; "allow this one grep" is a decision
// they can only rubber-stamp, and a turn that edits six files and runs a dozen
// commands asks a dozen times. See docs/adr/0005.
const (
	// OptionAllowSessionTool authorises every invocation of one tool, in one
	// session, until the grant expires.
	OptionAllowSessionTool = "allow-session-tool"
	// OptionAllowExact authorises one tool called with exactly these arguments,
	// in one session, until the grant expires. It is the narrow one, and the
	// right choice for a dangerous command that has been read carefully.
	OptionAllowExact = "allow-exact"
)

// Grant scopes.
const (
	// ScopeTool matches any invocation of the tool in the session.
	ScopeTool = "tool"
	// ScopeExact matches only the invocation whose arguments are identical.
	ScopeExact = "exact"
)

// Grant is a standing authorisation: a scope a human agreed to once, and the
// time it stops applying.
//
// Every grant is bounded on three axes and none of them is optional: it names
// one session, it names one tool (and for ScopeExact one exact invocation), and
// it expires. A grant that outlived the session it was made in would be an
// authorisation nobody remembers giving, for work they cannot see.
type Grant struct {
	ID string `json:"id"`
	// SessionID is the only session this applies to.
	SessionID string `json:"sessionId"`
	// Tool is the tool name the operator agreed to.
	Tool string `json:"tool"`
	// Scope is ScopeTool or ScopeExact.
	Scope string `json:"scope"`
	// Summary is a one-line reminder of what was agreed to, for ScopeExact. It
	// is display only — matching uses a digest, so a grant can never match
	// something other than the exact arguments that were on screen.
	Summary string `json:"summary,omitempty"`
	// CreatedAt and ExpiresAt bound it.
	CreatedAt time.Time `json:"createdAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Uses counts how many requests this grant has answered, so a list can show
	// which ones are actually doing work.
	Uses int `json:"uses"`

	// digest is the argument fingerprint for ScopeExact. Unexported because it
	// is a matching detail, not something a client should reason about.
	digest string
}

// expired reports whether the grant has run out at t.
func (g Grant) expired(t time.Time) bool { return !t.Before(g.ExpiresAt) }

// matches reports whether this grant answers a request.
//
// The argument comparison is byte equality on the raw input the harness
// reported. That is deliberately the strictest available rule: a grant that
// matched "the same command with different whitespace" would be one whose scope
// the operator cannot predict from what they were shown.
func (g Grant) matches(sessionID, tool, input string, now time.Time) bool {
	if g.SessionID != sessionID || g.Tool != tool || g.expired(now) {
		return false
	}
	if g.Scope == ScopeExact {
		return g.digest == digestOf(input)
	}
	return true
}

// digestOf fingerprints a request's arguments.
func digestOf(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

// grantSummary renders the first line of an invocation for a grant list.
func grantSummary(input string) string {
	line := strings.TrimSpace(input)
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = strings.TrimSpace(line[:index])
	}
	const limit = 120
	if len(line) > limit {
		cut := limit
		for cut > 0 && line[cut]&0xC0 == 0x80 {
			cut--
		}
		line = strings.TrimSpace(line[:cut]) + "…"
	}
	return line
}

// recordGrant stores a grant and returns it.
func (b *Broker) recordGrant(sessionID, tool, scope, input, decidedBy string) Grant {
	grant := Grant{
		ID:        "grant_" + digestOf(sessionID + "\x00" + tool + "\x00" + scope + "\x00" + input)[:24],
		SessionID: sessionID,
		Tool:      tool,
		Scope:     scope,
		CreatedAt: b.now().UTC(),
		ExpiresAt: b.now().UTC().Add(b.grantTTL),
	}
	if scope == ScopeExact {
		grant.digest = digestOf(input)
		grant.Summary = grantSummary(input)
	}

	b.mu.Lock()
	if b.grants == nil {
		b.grants = map[string]Grant{}
	}
	// The id is derived from the scope, so agreeing to the same scope twice
	// refreshes the existing grant rather than stacking a second identical rule
	// that a list would show twice.
	b.grants[grant.ID] = grant
	b.mu.Unlock()

	b.logger.Info("approval grant recorded",
		"grant", grant.ID, "session", sessionID, "tool", tool, "scope", scope,
		"expiresAt", grant.ExpiresAt.Format(time.RFC3339), "by", decidedBy)
	return grant
}

// matchGrant answers a request from a standing grant, if one applies.
//
// It returns the grant that answered, so a caller can say which decision is
// being applied instead of presenting an automated yes as a fresh one.
func (b *Broker) matchGrant(sessionID, tool, input string) (Grant, bool) {
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Expired grants are dropped here rather than by a sweeper: this is the only
	// place they can do harm, and a broker with no live grants should hold none.
	var best *Grant
	for id, grant := range b.grants {
		if grant.expired(now) {
			delete(b.grants, id)
			continue
		}
		if !grant.matches(sessionID, tool, input, now) {
			continue
		}
		// Prefer the narrowest match, so an exact grant's use count reflects the
		// decision the operator actually made rather than being absorbed by a
		// broader rule that also happens to apply.
		if best == nil || (grant.Scope == ScopeExact && best.Scope != ScopeExact) {
			candidate := grant
			best = &candidate
		}
	}
	if best == nil {
		return Grant{}, false
	}
	best.Uses++
	b.grants[best.ID] = *best
	return *best, true
}

// Grants lists the live grants, newest first.
func (b *Broker) Grants() []Grant {
	now := b.now()

	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]Grant, 0, len(b.grants))
	for id, grant := range b.grants {
		if grant.expired(now) {
			delete(b.grants, id)
			continue
		}
		out = append(out, grant)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// RevokeGrant removes a standing authorisation. It reports whether one was
// there, so a client can tell "revoked" from "already gone".
func (b *Broker) RevokeGrant(id string) bool {
	b.mu.Lock()
	_, ok := b.grants[id]
	delete(b.grants, id)
	b.mu.Unlock()
	if ok {
		b.logger.Info("approval grant revoked", "grant", id)
	}
	return ok
}

// Granted is the payload of events.TypeApprovalGranted: a request answered by a
// decision the operator made earlier, rather than by this prompt.
//
// It carries the tool and the arguments as well as the rule, because a client
// showing "auto-approved" in a transcript has to be able to say *what* was
// approved without a second lookup — and by then the request itself is gone.
type Granted struct {
	Grant Grant  `json:"grant"`
	Tool  string `json:"tool"`
	Input string `json:"input"`
}

// RevokeSessionGrants drops every grant made in one session.
//
// A grant is scoped to a session, so a session that is released, deleted or
// otherwise finished takes its authorisations with it. Without this, a rule
// agreed to for work that is over would sit there applying to whatever ran in
// that session next.
func (b *Broker) RevokeSessionGrants(sessionID string) int {
	b.mu.Lock()
	removed := 0
	for id, grant := range b.grants {
		if grant.SessionID != sessionID {
			continue
		}
		delete(b.grants, id)
		removed++
	}
	b.mu.Unlock()
	if removed > 0 {
		b.logger.Info("grants dropped with their session",
			"session", sessionID, "count", removed)
	}
	return removed
}

// Package sessionlog projects DeepSeek Harness's persisted session log into the
// transcript the mobile client renders.
//
// Why read the log at all: the ACP session listing returns only an id and a
// working directory, and `session/resume` explicitly does not replay history. A
// phone opening a conversation started at the desk would otherwise show nothing.
// The log is the only source of that history.
//
// This is the one place in the gateway that depends on a format DSH does not
// publish as a contract, so the coupling is deliberately quarantined and made
// safe:
//
//   - Access is strictly read-only. The gateway never takes the write lock and
//     never appends, so a format surprise cannot corrupt a session.
//   - The header carries a schema version. An unknown version yields
//     ErrUnsupportedFormat, which the API turns into an empty transcript with
//     "unsupported": true rather than an error, so the UI degrades to "open this
//     on your desktop" instead of breaking.
//   - Unknown event types are skipped, not rejected, so DSH adding a new event
//     kind does not blank the transcript.
//
// The event shapes implemented here were derived by decoding real session files,
// not from documentation.
package sessionlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/toolresult"
)

// SupportedVersion is the session log schema this build understands. It is the
// `version` field of the log's first line.
const SupportedVersion = 4

// logFileName is the compressed log inside a session directory.
const logFileName = "session.v4.jsonl.zstd"

// lockFileName is the writer lock DSH holds while a session is open.
const lockFileName = "session.lock"

// ErrUnsupportedFormat reports a log written by a newer DSH than this build
// understands. It is a normal, expected condition after a DSH upgrade, not a
// fault, and callers are expected to degrade rather than fail.
var ErrUnsupportedFormat = errors.New("sessionlog: unsupported session log format")

// Role classifies a transcript item.
type Role string

const (
	// RoleUser is a message from the operator.
	RoleUser Role = "user"
	// RoleAssistant is a committed model message.
	RoleAssistant Role = "assistant"
	// RoleTool is a tool invocation and its result.
	RoleTool Role = "tool"
	// RoleNotice is a system-level annotation such as a turn boundary.
	RoleNotice Role = "notice"
)

// The actors a notice can be about.
//
// This is the *transcript's* vocabulary, and it is deliberately wider than the
// notification's: a delegated child settles as a row in the conversation, so a
// reader scrolling history has to be told which agent a row belongs to, while a
// notification is only ever about the main agent or the gateway itself. See
// push.ActorKind for the other half.
const (
	// ActorSubagent marks a delegated child agent's own report.
	ActorSubagent = "subagent"
)

// OriginSubagent is how a session's log header classifies a delegated child:
// the harness writes `origin: "subagent"` for a session it started to answer a
// `subagent` call, and writes no origin at all for one a person opened.
//
// The lineage field beside it — `parentSession` — cannot stand in for this: a
// forked session inherits the same field and is still the operator's own
// session, so filtering on it would silence exactly the sessions a person is
// waiting on.
const OriginSubagent = "subagent"

// How a settled thing ended. They appear on a notice's Outcome so a client can
// colour a row without reading the prose.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeCancelled = "cancelled"
)

// sourceSubagentSettled is DSH's source kind for the notice it writes when a
// background child agent settles. The harness calls it out as a distinct kind so
// that a transcript never presents a runtime account as something the child
// wrote, and reading it is what lets this projection do the same.
const sourceSubagentSettled = "subagent-settled"

// Item is one rendered transcript entry.
type Item struct {
	// ID is stable across reads, so a client can key a list on it.
	ID string `json:"id"`
	// Seq is the log sequence number, used for paging.
	Seq int64 `json:"seq"`
	// Time is the event timestamp.
	Time time.Time `json:"time"`
	// Role discriminates the fields below.
	Role Role `json:"role"`

	// Text is the message body for user and assistant items, and a short
	// description for notices.
	Text string `json:"text,omitempty"`
	// Thinking is the model's committed reasoning, shown collapsed.
	Thinking string `json:"thinking,omitempty"`
	// Model names the model that produced an assistant message.
	Model string `json:"model,omitempty"`
	// Usage is per-message token accounting, when the log recorded it.
	Usage *Usage `json:"usage,omitempty"`

	// Tool fields, set when Role is RoleTool.
	Tool    string `json:"tool,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"isError,omitempty"`
	// Pending is true for a tool call whose result has not been recorded.
	Pending bool `json:"pending,omitempty"`
	// EndedAt is when the result was recorded, which is what makes a call's
	// duration knowable in history. It is a pointer because "the log does not
	// say" and "the log says the zero time" are different answers, and only the
	// first should be omitted.
	EndedAt *time.Time `json:"endedAt,omitempty"`
	// Facts is what the recorded result said about how the call ended: the exit
	// status, the harness's own error, whether it truncated its output. The
	// session log is the only place these exist for a call the gateway did not
	// drive, and isError alone does not carry them — DSH reports a non-zero exit
	// rather than erroring, so a failed command is recorded as a success.
	toolresult.Facts

	// Attachments is how many non-text blocks the message carried — a
	// screenshot, a pasted image. The image itself is deliberately not
	// projected: a transcript that carried every image anyone ever attached
	// would be tens of megabytes for a phone to scroll, and the log remains the
	// place to look one up in full. What matters for reading history is that a
	// prompt was not only its words.
	Attachments int `json:"attachments,omitempty"`

	// Actor names which agent a notice is about, when it is about one.
	//
	// It is empty for the app's own annotations and "subagent" for a delegated
	// task's settlement — the one case where the log records *who* finished. The
	// distinction is the whole point: a session can run several agents, and a
	// reader who cannot tell a child's report from the main agent's progress has
	// no way to know what just happened.
	Actor string `json:"actor,omitempty"`
	// Summary is the harness's own one-line account of a notice: for a
	// settlement, the sentence naming the child that settled. It is passed
	// through rather than re-derived, because the harness already wrote it and a
	// second wording would be a second thing to keep in step.
	Summary string `json:"summary,omitempty"`
	// Outcome is how the settled thing ended: "completed", "failed",
	// "cancelled". Empty for a notice that is not a settlement.
	Outcome string `json:"outcome,omitempty"`
}

// Usage is token accounting for one assistant message.
type Usage struct {
	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	TotalTokens     int `json:"totalTokens"`
	CacheReadTokens int `json:"cacheReadTokens,omitempty"`
}

// Meta is the session-level metadata the log carries.
type Meta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Model     string    `json:"model,omitempty"`
	// Effort is the reasoning effort the session ran at, as of its last model
	// selection.
	Effort string `json:"reasoningEffort,omitempty"`
	// TurnCount is how many turns the log records.
	TurnCount int `json:"turnCount"`
	// TurnRunning is true when the log's last turn boundary was a start: the
	// session is being written to right now, by whoever holds it.
	TurnRunning bool `json:"turnRunning,omitempty"`
	// TurnStartedAt is when the last turn boundary that was a start was
	// recorded. Zero means the log does not say — an older harness, or a
	// boundary written without a timestamp.
	//
	// It is what lets a follower state a turn's own duration and extent rather
	// than "since I started watching". It deliberately outlives the turn's end,
	// because the frame that settles a turn is about *that* turn: clearing it at
	// `turn/end` would leave the settlement with nothing to measure from.
	TurnStartedAt time.Time `json:"turnStartedAt,omitzero"`
	// Origin is the harness's own classification of the session, read from the
	// log header: OriginSubagent for a delegated child, empty for a session a
	// person opened. It is what tells the two apart downstream — a child's turn
	// is not the main agent's work, and nothing should announce it as though it
	// were.
	Origin string `json:"origin,omitempty"`

	// Preview is the opening thing the operator typed, truncated to one line.
	//
	// It exists because most sessions never get a title — DSH names a session
	// from its first turn, and a session that was created and left alone has
	// none — which leaves a list of "Untitled session" rows that say nothing.
	// The first prompt is what the reader actually recognises.
	Preview string `json:"preview,omitempty"`
	// Messages counts the rows a conversation would show: what the operator
	// typed and what the model answered. It is the cheapest honest signal of
	// whether a session holds any work, which is what a tidy-up needs.
	Messages int `json:"messages,omitempty"`
}

// Page is one page of transcript items.
type Page struct {
	// Items are ordered oldest first.
	Items []Item `json:"items"`
	// NextBefore pages further back; zero means there is nothing older.
	NextBefore int64 `json:"nextBefore,omitempty"`
	// Total is the number of items in the whole transcript.
	Total int `json:"total"`
}

// Store reads session logs under a root directory.
type Store struct {
	root   string
	logger *logx.Logger
	now    func() time.Time

	dec *zstd.Decoder

	// cache holds projections keyed by session id. A session log is append-only,
	// so a size and mtime match means the cached projection is still valid.
	mu    sync.Mutex
	cache map[string]*cached
	// maxCache bounds how many sessions are retained, so a long-lived gateway
	// does not hold every transcript it has ever rendered.
	maxCache int

	// prices are the unit prices a receipt may estimate cost from, configured
	// rather than fetched: the daemon has no business shelling out to a CLI that
	// holds the account's credentials.
	prices []Price

	// paths remembers where a session's log was found. Resolving an id otherwise
	// costs a glob of the whole tree — once per session, on every list request —
	// which is the difference between a list that costs one directory scan and
	// one that costs thousands of stats.
	//
	// It is a memo, so forgetting an entry costs one glob rather than
	// correctness — which is why it is capped like the other two tables instead
	// of being kept for every session the store has ever seen. pathOrder is the
	// ids in the order they were first resolved; see rememberPathLocked.
	paths     map[string]string
	pathOrder []string
	maxPaths  int

	// metas holds metadata only, which is what the session list needs for every
	// session the operator owns. Projections are far too large to keep for all of
	// them — decoding hundreds of logs per list request is what made the list
	// take half a second — but a metadata record is a few hundred bytes, so all
	// of them fit and an unchanged log is never read twice.
	metas    map[string]*cachedMeta
	maxMetas int
	order    atomic.Int64

	// decodes counts how many logs have been read, and decoded counts the
	// compressed bytes those reads consumed. They are diagnostics: the first is
	// what a session list costs, the second is what keeping a projection current
	// costs, and a test can hold either still.
	decodes atomic.Uint64
	decoded atomic.Uint64
}

// cached is the projection of one session.
//
// It is two things at once, under one lock: the state a later read resumes from,
// and the snapshot callers read. A fold now continues in place rather than
// producing a replacement value — the parser accumulates, and the parser that
// holds the rows a caller was handed is the same one the next append feeds — so
// what callers get is what was published rather than what is shared: items is
// replaced by a fresh slice on every fold and never written into afterwards.
//
// That costs one copy of the slice, not of the transcript: an item is a row of
// headers over strings both copies point at, so the duplicate is what a
// projection of a hundred thousand rows costs in pointers.
type cached struct {
	// mu guards every field below, and is held for the length of a fold. Per
	// session rather than per store, because folding one log has no business
	// stopping the store from reading another.
	mu sync.Mutex

	// size and modTime describe the file the fold has consumed. size is the
	// offset it committed, which is the file's length except while a flush is in
	// progress — deliberately, because a cache hit is exactly the claim that the
	// file has not moved on.
	size    int64
	modTime time.Time

	items []Item
	meta  Meta

	// parser and progress are where the next read continues; parser is nil until
	// the first fold.
	parser   *parser
	progress progress

	// order is when the entry was last folded, which is what eviction forgets
	// first. Atomic so eviction can read it while scanning under the store's
	// lock, without taking every entry's.
	order atomic.Int64
}

type cachedMeta struct {
	// mu guards every field below, for the same reason cached.mu does.
	mu sync.Mutex

	size     int64
	modTime  time.Time
	meta     Meta
	progress progress
	order    atomic.Int64
}

// covers reports whether this projection already describes the file info
// reports. The caller holds c.mu.
func (c *cached) covers(info os.FileInfo) bool {
	return c.size == info.Size() && c.modTime.Equal(info.ModTime())
}

// snapshot returns the published projection.
//
// The slice is replaced by every fold and never written into afterwards, so a
// caller may keep reading it for as long as it likes: what it will not see is
// the rows of a later fold.
func (c *cached) snapshot() ([]Item, Meta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items, c.meta
}

// rewind forgets everything folded so far, for a log that is no longer the one
// the fold was reading. The published snapshot goes with it: rows of a file that
// is no longer there are not something to hand out, and the fold that follows
// republishes. The caller holds c.mu.
func (c *cached) rewind() {
	c.parser = nil
	c.progress = progress{}
	c.items = nil
	c.meta = Meta{}
}

// covers reports whether this record already describes the file info reports.
// The caller holds m.mu.
func (m *cachedMeta) covers(info os.FileInfo) bool {
	return m.size == info.Size() && m.modTime.Equal(info.ModTime())
}

// rewind forgets the folded metadata, for a log that is no longer the one it was
// read from. The caller holds m.mu.
func (m *cachedMeta) rewind() {
	m.progress = progress{}
	m.meta = Meta{}
}

// New opens a store rooted at the DSH sessions directory.
func New(root string, logger *logx.Logger) (*Store, error) {
	if root == "" {
		return nil, errors.New("sessionlog: root is required")
	}
	// DecodeAll is documented as safe for concurrent use on a Decoder created
	// with a nil reader, which is why one decoder is shared.
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
	if err != nil {
		return nil, fmt.Errorf("sessionlog: create zstd decoder: %w", err)
	}
	return &Store{
		root:     root,
		logger:   logger,
		now:      time.Now,
		dec:      dec,
		cache:    map[string]*cached{},
		paths:    map[string]string{},
		maxCache: 16,
		maxPaths: 4096,
		metas:    map[string]*cachedMeta{},
		maxMetas: 4096,
	}, nil
}

// Close releases decoder resources.
func (s *Store) Close() {
	if s.dec != nil {
		s.dec.Close()
	}
}

// Meta returns a session's metadata.
//
// This is the call the session list makes once per session, so it is served from
// a metadata cache that holds every session rather than from the projection
// cache that holds a few transcripts. Only a log that actually changed is read
// again, only the frames it changed by are decoded, and reading it for metadata
// does not retain its rows.
//
// The two caches are never held at once: a record is locked, read or folded, and
// released before the other is touched. Nothing needs both, and taking them in
// one order here and the other in load is how a deadlock is written.
func (s *Store) Meta(ctx context.Context, sessionID string) (Meta, error) {
	path, info, err := s.stat(sessionID)
	if err != nil {
		return Meta{}, err
	}

	m := s.metaRecord(sessionID)
	m.mu.Lock()
	if m.covers(info) {
		meta := m.meta
		m.mu.Unlock()
		return meta, nil
	}
	m.mu.Unlock()

	// A projection read a moment ago already answered this, and it can hand over
	// more than the answer: its resume point is where a metadata-only fold of the
	// same file would have stopped too, because both readers fold the same events
	// the same way.
	if c := s.projection(sessionID, false); c != nil {
		c.mu.Lock()
		covers, meta, at := c.covers(info), c.meta, c.progress
		c.mu.Unlock()
		if covers {
			m.mu.Lock()
			m.publish(meta, at, info, s.next())
			m.mu.Unlock()
			return meta, nil
		}
	}

	if err := ctx.Err(); err != nil {
		return Meta{}, errx.Wrap(err, errx.KindTimeout, "cancelled", "the request was cancelled")
	}

	s.decodes.Add(1)
	m.mu.Lock()
	err = s.foldMeta(m, sessionID, path, info)
	meta := m.meta
	m.mu.Unlock()
	if err != nil {
		return Meta{}, err
	}
	s.evictMetas()
	return meta, nil
}

// MetaAt reads the metadata of a log at an explicit path.
//
// It exists for sessions that are no longer under the sessions root — a trashed
// one — where the id alone cannot be resolved. Nothing here is cached: a path
// outside the store has no stable identity to cache against.
func (s *Store) MetaAt(dir, sessionID string) (Meta, error) {
	path := filepath.Join(dir, logFileName)
	info, err := os.Stat(path)
	if err != nil {
		return Meta{}, errx.Wrap(err, errx.KindNotFound, "transcript_not_found", "")
	}
	s.decodes.Add(1)
	p := newParser(sessionID, s.logger, false)
	at, err := s.readFrom(path, progress{}, p.feed)
	if err != nil && at.empty() {
		return Meta{}, err
	}
	if err != nil {
		s.debug("sessionlog: partial read", sessionID, err)
	}
	_, meta, err := p.finish()
	if err != nil {
		return Meta{}, err
	}
	meta.UpdatedAt = info.ModTime().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = meta.UpdatedAt
	}
	return meta, nil
}

// Decodes reports how many logs this store has read and folded. It is the cost
// of a session list, made visible: the list must not grow with the number of
// sessions on disk, only with the number that changed.
func (s *Store) Decodes() uint64 { return s.decodes.Load() }

// DecodedBytes reports how many compressed bytes this store has decoded. It is
// the other half of Decodes: a log that changed costs the frames appended to it,
// not its whole length, and this is the number that says so.
func (s *Store) DecodedBytes() uint64 { return s.decoded.Load() }

// next hands out eviction order numbers.
func (s *Store) next() int64 { return s.order.Add(1) }

// publish records a fold's result on a metadata record. The caller holds m.mu.
func (m *cachedMeta) publish(meta Meta, at progress, info os.FileInfo, order int64) {
	m.meta = meta
	m.progress = at
	m.size = at.committed
	m.modTime = info.ModTime()
	m.order.Store(order)
}

// projection returns a session's projection entry, creating it when asked to.
//
// A nil answer means nothing has projected that session yet, which is what lets
// Meta skip the projection cache instead of filling it.
func (s *Store) projection(sessionID string, create bool) *cached {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[sessionID]
	if !ok && !create {
		return nil
	}
	if !ok {
		c = &cached{}
		s.cache[sessionID] = c
	}
	return c
}

// metaRecord returns a session's metadata record, creating it if needed.
func (s *Store) metaRecord(sessionID string) *cachedMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[sessionID]
	if !ok {
		m = &cachedMeta{}
		s.metas[sessionID] = m
	}
	return m
}

// debug logs a read worth explaining but not worth failing.
func (s *Store) debug(msg, sessionID string, err error) {
	if s.logger != nil {
		s.logger.Debug(msg, "session", sessionID, "error", err.Error())
	}
}

// evictMetas drops the oldest metadata records once the bound is exceeded.
//
// A record being folded is skipped rather than dropped: its work would be lost
// and the same log folded twice. Skipping can leave the map above its bound
// until that fold finishes, which is bounded by the number of concurrent readers
// rather than by the number of sessions.
func (s *Store) evictMetas() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.metas) > s.maxMetas {
		victim := ""
		var oldest int64
		for id, m := range s.metas {
			if !m.mu.TryLock() {
				continue
			}
			m.mu.Unlock()
			if order := m.order.Load(); victim == "" || order < oldest {
				victim, oldest = id, order
			}
		}
		if victim == "" {
			return
		}
		delete(s.metas, victim)
	}
}

// evict drops the oldest projections once the cache exceeds its bound, under the
// same rule as evictMetas: an entry being folded is skipped rather than thrown
// away, because a fold that is discarded is a whole log decoded again.
func (s *Store) evict() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.cache) > s.maxCache {
		victim := ""
		var oldest int64
		for id, c := range s.cache {
			if !c.mu.TryLock() {
				continue
			}
			c.mu.Unlock()
			if order := c.order.Load(); victim == "" || order < oldest {
				victim, oldest = id, order
			}
		}
		if victim == "" {
			return
		}
		delete(s.cache, victim)
	}
}

// Transcript returns a page of items ending before the given sequence number.
//
// before == 0 starts at the newest item. Items are returned oldest first so the
// client can append them to the top of its list without re-sorting.
func (s *Store) Transcript(ctx context.Context, sessionID string, before int64, limit int) (Page, error) {
	c, err := s.load(ctx, sessionID)
	if err != nil {
		return Page{}, err
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	published, _ := c.snapshot()
	items := published
	if before > 0 {
		// Items are ordered by sequence, so a binary search finds the cut.
		cut := sort.Search(len(items), func(i int) bool { return items[i].Seq >= before })
		items = items[:cut]
	}

	// Total is the whole transcript, not the page: it is what tells the client
	// how much history is behind the page it is holding.
	page := Page{Total: len(published)}
	if len(items) <= limit {
		page.Items = append([]Item(nil), items...)
		return page, nil
	}

	start := len(items) - limit
	page.Items = append([]Item(nil), items[start:]...)
	// The caller pages backwards by passing the oldest sequence it received.
	page.NextBefore = page.Items[0].Seq
	return page, nil
}

// Receipt summarises what a session did, from its own log.
func (s *Store) Receipt(ctx context.Context, sessionID string) (Receipt, error) {
	items, meta, err := s.Items(ctx, sessionID)
	if err != nil {
		return Receipt{}, err
	}
	return Summarise(items, meta).WithCost(s.prices), nil
}

// SetPrices installs the unit prices a receipt may estimate cost from.
func (s *Store) SetPrices(prices []Price) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices = prices
}

// Items returns the whole projection and its metadata.
//
// This is the reader for callers that need to compare one read against the next
// — the live follower, which turns the difference into events — rather than
// render a page of history. The slice is a published snapshot: it is never
// written into after it is returned, so a caller may keep reading it for as long
// as it likes, and what it will not see is the rows a later read adds.
func (s *Store) Items(ctx context.Context, sessionID string) ([]Item, Meta, error) {
	c, err := s.load(ctx, sessionID)
	if err != nil {
		return nil, Meta{}, err
	}
	items, meta := c.snapshot()
	return items, meta, nil
}

// Locked reports whether some process currently holds a session's writer lock.
//
// DSH keeps that lock for as long as it has the session open, so a held lock
// means the session has an owner right now: the desktop, a headless run, or this
// gateway's own child. The check never blocks and never keeps the lock — it
// asks, and lets go.
//
// It is used to decide whether a log that ends mid-turn is a turn still being
// written or one whose process died and never wrote its end.
func (s *Store) Locked(sessionID string) bool {
	path, err := s.logPath(sessionID)
	if err != nil {
		return false
	}
	return lockHeld(filepath.Join(filepath.Dir(path), lockFileName))
}

// Log is one session log as it exists on disk right now.
//
// The size and modification time are what a follower needs to decide whether a
// session is worth reading at all: with hundreds of sessions and one being
// written, reading every projection every second is the difference between
// noticing a turn immediately and noticing it long after it ended.
type Log struct {
	// ID is the session the log belongs to.
	ID string
	// Path is where the log file lives.
	Path string
	// Size and ModTime are what the file says. The list compares them against
	// what it read last time to decide whether the log is worth re-reading.
	Size    int64
	ModTime time.Time
}

// Logs lists the session logs under the root.
//
// It answers "what could be followed", not "what is running": the log is the
// only artifact a session leaves when its process is gone.
func (s *Store) Logs() []Log {
	dirs, err := filepath.Glob(filepath.Join(s.root, "*", "*"))
	if err != nil {
		return nil
	}
	logs := make([]Log, 0, len(dirs))
	found := make(map[string]string, len(dirs))
	for _, dir := range dirs {
		id := filepath.Base(dir)
		if !validSessionID(id) {
			continue
		}
		path := filepath.Join(dir, logFileName)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		logs = append(logs, Log{ID: id, Path: path, Size: info.Size(), ModTime: info.ModTime()})
		found[id] = path
	}
	sort.Slice(logs, func(i, j int) bool { return logs[i].ID < logs[j].ID })

	// One scan answers every later lookup.
	s.mu.Lock()
	for id, path := range found {
		s.rememberPathLocked(id, path)
	}
	s.mu.Unlock()

	return logs
}

// rememberPathLocked records where a session's log lives, dropping the oldest
// memo once the table is at its cap. The caller holds s.mu.
//
// The path is found by globbing the sessions root, and the watcher runs that
// scan every second — so without a cap, one entry per session that has ever
// existed stays here for the life of the process, whether or not the session
// still does.
func (s *Store) rememberPathLocked(sessionID, path string) {
	if _, exists := s.paths[sessionID]; !exists {
		s.pathOrder = append(s.pathOrder, sessionID)
	}
	s.paths[sessionID] = path

	for len(s.pathOrder) > s.maxPaths {
		oldest := s.pathOrder[0]
		s.pathOrder = s.pathOrder[1:]
		delete(s.paths, oldest)
	}
}

// forgetPathLocked drops a session's memo and its place in the order. The caller
// holds s.mu.
func (s *Store) forgetPathLocked(sessionID string) {
	delete(s.paths, sessionID)
	for index, id := range s.pathOrder {
		if id == sessionID {
			s.pathOrder = append(s.pathOrder[:index], s.pathOrder[index+1:]...)
			return
		}
	}
}

// stat resolves a session id to its log file and current size and mtime.
func (s *Store) stat(sessionID string) (string, os.FileInfo, error) {
	path, err := s.logPath(sessionID)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, errx.New(errx.KindNotFound, "transcript_not_found",
				"no stored transcript exists for that session")
		}
		return "", nil, errx.Wrap(err, errx.KindInternal, "transcript_unreadable", "")
	}
	return path, info, nil
}

// load returns the projection for a session, folding whatever its log has grown
// by since the last read.
func (s *Store) load(ctx context.Context, sessionID string) (*cached, error) {
	path, info, err := s.stat(sessionID)
	if err != nil {
		return nil, err
	}

	c := s.projection(sessionID, true)
	c.mu.Lock()
	if c.covers(info) {
		c.mu.Unlock()
		return c, nil
	}

	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, errx.Wrap(err, errx.KindTimeout, "cancelled", "the request was cancelled")
	}

	s.decodes.Add(1)
	err = s.foldProjection(c, sessionID, path, info)
	meta, at := c.meta, c.progress
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// A projection answers the metadata question too, so the record beside it
	// stays current without another read.
	m := s.metaRecord(sessionID)
	m.mu.Lock()
	m.publish(meta, at, info, s.next())
	m.mu.Unlock()

	s.evict()
	s.evictMetas()
	return c, nil
}

// logPath resolves a session id to its log file, rejecting anything that could
// escape the sessions root.
//
// Session ids reach this function straight from an HTTP path, so the validation
// is a security control, not a formality: without it "..%2f..%2fetc%2fpasswd"
// would be a file read primitive.
func (s *Store) logPath(sessionID string) (string, error) {
	if !validSessionID(sessionID) {
		return "", errx.New(errx.KindInvalid, "invalid_session_id", "that session id is not valid")
	}

	// The answer is remembered, so a session's path is searched for once rather
	// than once per request.
	s.mu.Lock()
	cached, ok := s.paths[sessionID]
	s.mu.Unlock()
	if ok {
		if _, err := os.Stat(cached); err == nil {
			return cached, nil
		}
	}

	// DSH stores sessions one directory per workspace, named after the workspace
	// path with separators rewritten. The session directory itself is named
	// exactly after the session id.
	matches, err := filepath.Glob(filepath.Join(s.root, "*", sessionID))
	if err != nil {
		return "", errx.Wrap(err, errx.KindInternal, "transcript_lookup_failed", "")
	}
	for _, dir := range matches {
		// Defence in depth: even with a validated id, confirm the resolved path
		// is inside the root before touching it.
		rel, err := filepath.Rel(s.root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		path := filepath.Join(dir, logFileName)
		if _, err := os.Stat(path); err == nil {
			s.mu.Lock()
			s.rememberPathLocked(sessionID, path)
			s.mu.Unlock()
			return path, nil
		}
	}
	return "", errx.New(errx.KindNotFound, "transcript_not_found",
		"no stored transcript exists for that session")
}

// validSessionID accepts the identifier shapes DSH produces: prefixed ids such as
// "session-<uuid>" and bare UUIDs. Anything containing a path separator, a
// traversal segment, or an unusual character is refused.
func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return false
		}
	}
	// A leading dot would allow "." and ".." style names.
	return id[0] != '.' && id[0] != '-'
}

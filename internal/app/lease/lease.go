// Package lease manages the lifetime of attached harness sessions.
//
// Why this exists: DeepSeek Harness enforces one live writer per session with a
// kernel lock on the session's log file. A session attached to the gateway's ACP
// child therefore cannot be opened on the desktop, and vice versa. If the gateway
// simply attached sessions and kept them, opening your laptop would show "already
// open elsewhere" for a conversation you last touched on your phone hours ago.
//
// A lease is therefore held on a short, self-renewing timer rather than for the
// life of a client connection:
//
//   - Attaching refreshes the lease. So does any client activity, a WebSocket
//     subscribe, a heartbeat, a metadata fetch.
//   - An in-flight turn pins the lease regardless of activity, because releasing
//     mid-turn would abandon the agent's work.
//   - Anything else is released after IdleTimeout, which hands the session back
//     to the desktop.
//
// Liveness is expressed as a last-used timestamp rather than a subscriber count
// on purpose: a phone that walks into a lift never sends a clean goodbye, and a
// refcount would leak the lease forever.
package lease

import (
	"context"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// Manager owns the set of attached sessions.
type Manager struct {
	harness harness.Harness
	idle    time.Duration
	logger  *logx.Logger
	now     func() time.Time

	// onAcquire and onRelease let the caller publish session.state events
	// without this package depending on the event bus.
	onAcquire func(info harness.SessionInfo)
	onRelease func(sessionID string, reason string)

	mu     sync.Mutex
	leases map[string]*entry
}

type entry struct {
	info     harness.SessionInfo
	config   []harness.ConfigOption
	lastUsed time.Time
	busy     bool
	// pinned keeps a lease alive beyond the idle window. It is set while a
	// client explicitly asks to hold the session, for example while the desktop
	// is known to be closed and the operator is working entirely from the phone.
	pinned bool
}

// Options configures a Manager.
type Options struct {
	Harness harness.Harness
	// IdleTimeout is how long an unused, unpinned, non-busy lease survives.
	IdleTimeout time.Duration
	Logger      *logx.Logger
	// OnAcquire and OnRelease are optional observers.
	OnAcquire func(info harness.SessionInfo)
	OnRelease func(sessionID string, reason string)
	Now       func() time.Time
}

// New builds a Manager.
func New(opts Options) *Manager {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	return &Manager{
		harness:   opts.Harness,
		idle:      opts.IdleTimeout,
		logger:    opts.Logger,
		now:       opts.Now,
		onAcquire: opts.OnAcquire,
		onRelease: opts.OnRelease,
		leases:    map[string]*entry{},
	}
}

// Snapshot describes one lease for the API.
type Snapshot struct {
	SessionID string
	Busy      bool
	Pinned    bool
	LastUsed  time.Time
	Config    []harness.ConfigOption
}

// Acquire attaches a session, or refreshes it when already attached.
//
// workspace is required only when the session is not yet attached, because DSH
// verifies the physical directory on resume.
func (m *Manager) Acquire(ctx context.Context, sessionID, workspace string) (harness.SessionInfo, error) {
	m.mu.Lock()
	if e, ok := m.leases[sessionID]; ok {
		e.lastUsed = m.now()
		info := e.info
		m.mu.Unlock()
		return info, nil
	}
	m.mu.Unlock()

	session, err := m.harness.ResumeSession(ctx, sessionID, workspace)
	if err != nil {
		return harness.SessionInfo{}, err
	}

	m.mu.Lock()
	// A concurrent Acquire may have won the race. Resuming twice would make DSH
	// reject the second attempt, so adopt the existing entry instead.
	if e, ok := m.leases[sessionID]; ok {
		e.lastUsed = m.now()
		info := e.info
		m.mu.Unlock()
		return info, nil
	}
	m.leases[sessionID] = &entry{
		info:     session.Info,
		config:   session.Config,
		lastUsed: m.now(),
	}
	m.mu.Unlock()

	if m.onAcquire != nil {
		m.onAcquire(session.Info)
	}
	m.logger.Info("session leased", "session", sessionID, "workspace", session.Info.Workspace)
	return session.Info, nil
}

// Adopt records a session the harness already attached, such as one just created.
func (m *Manager) Adopt(session harness.Session) {
	m.mu.Lock()
	m.leases[session.Info.ID] = &entry{
		info:     session.Info,
		config:   session.Config,
		lastUsed: m.now(),
	}
	m.mu.Unlock()

	if m.onAcquire != nil {
		m.onAcquire(session.Info)
	}
}

// Touch refreshes a lease if it exists. It is called on any client activity.
func (m *Manager) Touch(sessionID string) {
	m.mu.Lock()
	if e, ok := m.leases[sessionID]; ok {
		e.lastUsed = m.now()
	}
	m.mu.Unlock()
}

// SetBusy pins or unpins a lease for the duration of a turn.
func (m *Manager) SetBusy(sessionID string, busy bool) {
	m.mu.Lock()
	if e, ok := m.leases[sessionID]; ok {
		e.busy = busy
		e.lastUsed = m.now()
	}
	m.mu.Unlock()
}

// SetPinned keeps a lease alive indefinitely regardless of idle time.
func (m *Manager) SetPinned(sessionID string, pinned bool) {
	m.mu.Lock()
	if e, ok := m.leases[sessionID]; ok {
		e.pinned = pinned
		e.lastUsed = m.now()
	}
	m.mu.Unlock()
}

// SetConfig records configuration changes so Get reports current values.
func (m *Manager) SetConfig(sessionID string, config []harness.ConfigOption) {
	m.mu.Lock()
	if e, ok := m.leases[sessionID]; ok {
		e.config = config
	}
	m.mu.Unlock()
}

// IsLeased reports whether the session is currently attached.
func (m *Manager) IsLeased(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.leases[sessionID]
	return ok
}

// IsBusy reports whether a turn is in flight for the session.
func (m *Manager) IsBusy(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.leases[sessionID]
	return ok && e.busy
}

// Get returns a lease snapshot.
func (m *Manager) Get(sessionID string) (Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.leases[sessionID]
	if !ok {
		return Snapshot{}, false
	}
	return Snapshot{
		SessionID: sessionID,
		Busy:      e.busy,
		Pinned:    e.pinned,
		LastUsed:  e.lastUsed,
		Config:    e.config,
	}, true
}

// List returns every held lease.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, 0, len(m.leases))
	for id, e := range m.leases {
		out = append(out, Snapshot{
			SessionID: id,
			Busy:      e.busy,
			Pinned:    e.pinned,
			LastUsed:  e.lastUsed,
			Config:    e.config,
		})
	}
	return out
}

// Release detaches a session, handing it back to the desktop.
//
// It is safe to call for a session that is not attached. A busy session is still
// released if force is set — that is what an operator pressing "release" means —
// but the in-flight turn is abandoned as a result, so callers should warn first.
func (m *Manager) Release(ctx context.Context, sessionID, reason string, force bool) error {
	m.mu.Lock()
	e, ok := m.leases[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	if e.busy && !force {
		m.mu.Unlock()
		return errx.New(errx.KindConflict, "session_busy",
			"a turn is in flight; cancel it before releasing the session")
	}
	delete(m.leases, sessionID)
	m.mu.Unlock()

	// Close outside the lock: DSH's close is a round trip, and holding the mutex
	// across it would stall every other session operation.
	err := m.harness.CloseSession(ctx, sessionID)
	if err != nil {
		m.logger.Warn("session close failed", "session", sessionID, "error", err.Error())
	}
	if m.onRelease != nil {
		m.onRelease(sessionID, reason)
	}
	m.logger.Info("session released", "session", sessionID, "reason", reason)
	return err
}

// ReleaseAll detaches every session. It is used on shutdown.
func (m *Manager) ReleaseAll(ctx context.Context, reason string) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.leases))
	for id := range m.leases {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		if err := m.Release(ctx, id, reason, true); err != nil {
			m.logger.Warn("release on shutdown failed", "session", id, "error", err.Error())
		}
	}
}

// Run reaps idle leases until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	// Check four times per idle window so the reported idle time stays close to
	// the configured value without polling hard.
	interval := m.idle / 4
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	if interval > time.Minute {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reap(ctx)
		}
	}
}

// reap releases every lease that has gone idle.
func (m *Manager) reap(ctx context.Context) {
	now := m.now()

	m.mu.Lock()
	var expired []string
	for id, e := range m.leases {
		// A busy or pinned lease is never reclaimed by the reaper. Busy means an
		// agent is mid-turn; pinned means a human asked to keep it.
		if e.busy || e.pinned {
			continue
		}
		if now.Sub(e.lastUsed) >= m.idle {
			expired = append(expired, id)
		}
	}
	m.mu.Unlock()

	for _, id := range expired {
		if err := m.Release(ctx, id, "idle", false); err != nil {
			// Release re-checks busy under the lock, so a turn that started
			// between the scan and here is safely skipped.
			continue
		}
		m.logger.Info("released idle session so the desktop can open it", "session", id)
	}
}

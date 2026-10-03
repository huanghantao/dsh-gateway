package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/huanghantao/dsh-gateway/internal/app/approvals"
	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// eventsHandler serves the multiplexed event stream.
//
// The connection is the client's window onto every session at once, so it must
// survive radio changes, backgrounded tabs, sleeping phones, and — the case this
// file exists for — the gateway being redeployed underneath it. Four details
// make that work:
//
//   - A cursor of (generation, seq), so a reconnect can be told apart from a
//     cursor that belongs to a process which no longer exists.
//   - Replay from that cursor, so a reconnect does not lose output.
//   - A resync *and a snapshot* when the cursor cannot be honoured, so a client
//     that reconnects across a restart is told what the state is rather than
//     only that its copy of it is wrong.
//   - Server-side pings, so an idle tunnel is kept open and a dead peer is
//     noticed.
//
// The invariant the whole design rests on: there is no outcome in which the
// connection looks healthy and the client is silently missing frames. Every
// cursor either resumes exactly or produces a resync.
type eventsHandler struct {
	server *Server

	// mu guards writes: coder/websocket permits only one concurrent writer.
	mu  sync.Mutex
	ws  *websocket.Conn
	ctx context.Context

	// subMu guards the session filter, which the client mutates at runtime.
	subMu   sync.RWMutex
	filters map[string]struct{}
}

// clientMessage is a frame sent by the client.
type clientMessage struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId,omitempty"`
}

// handleEvents upgrades the connection and pumps the event stream.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Authenticate before the upgrade so an unauthenticated peer gets an ordinary
	// 401 problem document rather than a WebSocket close it cannot interpret.
	cred, ok := s.extractCredential(r)
	if !ok {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindUnauthenticated, "authentication_required",
				"the event stream requires a paired device"))
		return
	}
	principal, err := s.deps.Auth.Authenticate(r.Context(), cred)
	if err != nil {
		s.deps.Limiter.Fail(clientKey(r))
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()), err)
		return
	}
	// A browser always sends Origin on a WebSocket handshake, so this is a real
	// cross-site defence rather than a formality.
	if !s.originAllowed(r) {
		httpcore.WriteError(w, r, s.deps.Logger, httpcore.RequestIDFrom(r.Context()),
			errx.New(errx.KindForbidden, "origin_rejected",
				"the handshake did not originate from the gateway"))
		return
	}

	h := &eventsHandler{
		server:  s,
		ctx:     r.Context(),
		filters: map[string]struct{}{},
	}
	for _, id := range r.URL.Query()["session"] {
		if id != "" {
			h.filters[id] = struct{}{}
		}
	}

	cursor := parseCursor(r)

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The origin policy is enforced above, against the Host the browser
		// actually used, which is the same rule the rest of the API applies.
		// Delegating to the library's pattern matcher here would risk the two
		// policies drifting apart.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		// Accept already wrote a response.
		s.deps.Logger.Debug("websocket upgrade failed", "error", err.Error())
		return
	}
	h.ws = ws

	s.deps.Logger.Info("event stream opened",
		"device", principal.DeviceID,
		"since", cursor.Seq,
		"generation", shortGeneration(cursor.Generation),
		"client_ip", clientKey(r))
	defer func() {
		_ = ws.CloseNow()
		s.deps.Logger.Info("event stream closed", "device", principal.DeviceID)
	}()

	// A generous read limit: clients send only small control frames, so a large
	// frame means something is wrong.
	ws.SetReadLimit(64 << 10)

	// Subscribe before taking the snapshot, and label the snapshot with the
	// watermark observed here. The order is what makes the two halves compose:
	// everything published before this line is either replayed or superseded by
	// the snapshot, and everything published after it arrives on the stream with
	// a sequence number greater than the one the snapshot claims. A duplicate is
	// possible in the overlap and is harmless — every frame here is idempotent —
	// whereas a gap would not be.
	resume := s.deps.Bus.Subscribe(cursor, h.match)
	defer resume.Subscription.Close()
	snapshotAt := s.deps.Bus.Watermark()

	// Bound the lifetime so that a forgotten tab does not hold a connection (and
	// a bus subscription) open indefinitely. Clients reconnect with their cursor,
	// so this is invisible to them.
	ctx, cancel := context.WithTimeout(h.ctx, 12*time.Hour)
	defer cancel()

	// Reading is handled in its own goroutine because the write path blocks on
	// the network; running them in sequence would mean a client that stops
	// reading also stops being able to send.
	go h.readLoop(ctx) //nolint:contextcheck // connected to the client, not to a request

	if err := h.writeJSON(ctx, map[string]any{ //nolint:contextcheck // see above
		"type": events.TypeHello,
		"data": map[string]any{
			"deviceId":   principal.DeviceID,
			"generation": s.deps.Bus.Generation(),
			"replayFrom": resume.From,
			"serverTime": time.Now().UTC(),
		},
	}); err != nil {
		return
	}

	if !resume.Resumable() {
		// A first connection and an unusable cursor are both repaired with a
		// snapshot, and they are not the same thing to a client. A resync is an
		// instruction — "everything you are showing is suspect, refetch it" —
		// and it costs three requests. A client that has never held a cursor is
		// showing nothing yet: bootstrap is about to fetch its state anyway, and
		// telling it to refetch is telling it to do what it is already doing.
		//
		// So only a cursor that existed and could not be honoured earns one.
		if resume.Reason != events.ResyncNoCursor {
			// The order matters: the resync comes first, so a client that renders
			// frames as they arrive has already been told its copy is suspect
			// before the replacement for it arrives.
			if err := h.writeJSON(ctx, events.Event{ //nolint:contextcheck // see above
				Time: time.Now().UTC(),
				Type: events.TypeResync,
				Data: map[string]any{"reason": resume.Reason.String()},
			}); err != nil {
				return
			}
			s.deps.Logger.Info("event stream resynchronised",
				"device", principal.DeviceID,
				"reason", resume.Reason.String(),
				"snapshotAt", snapshotAt)
		}
		if err := h.writeJSON(ctx, events.Event{ //nolint:contextcheck // see above
			Seq:  snapshotAt,
			Time: time.Now().UTC(),
			Type: events.TypeSnapshot,
			Data: s.snapshot(h.scope(), snapshotAt),
		}); err != nil {
			return
		}
	}

	h.pump(ctx, resume.Subscription) //nolint:contextcheck // connected to the client, not to a request
}

// parseCursor reads the client's position from the query string.
//
// A malformed value is treated as no cursor rather than as an error: the query
// string is not the place to fail a connection, and "no cursor" already has a
// defined, safe meaning — resync and snapshot.
func parseCursor(r *http.Request) events.Cursor {
	q := r.URL.Query()
	cursor := events.Cursor{Generation: q.Get("generation")}
	if raw := q.Get("since"); raw != "" {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
			cursor.Seq = n
		}
	}
	return cursor
}

// shortGeneration abbreviates a generation for a log line. It is a label, not a
// secret, but a full one in every log record is noise.
func shortGeneration(g string) string {
	if len(g) > 8 {
		return g[:8]
	}
	if g == "" {
		return "-"
	}
	return g
}

// pump forwards bus events until the connection ends.
func (h *eventsHandler) pump(ctx context.Context, sub *events.Subscription) {
	// A heartbeat keeps intermediaries from reaping an idle tunnel. Caddy and frp
	// both have their own idle handling, and a phone's radio may silently drop a
	// flow that looks quiet.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case <-ping.C:
			if err := h.ping(ctx); err != nil {
				return
			}
		case e, ok := <-sub.Events():
			if !ok {
				return
			}
			// Falling behind is discovered here rather than at reconnect: this
			// client is connected and still lost frames, so it needs both halves
			// of the repair immediately.
			if reason := sub.TakeResync(); reason != "" {
				if err := h.resync(ctx, reason); err != nil {
					return
				}
			}
			if err := h.writeJSON(ctx, e); err != nil {
				return
			}
		}
	}
}

// resync tells a connected client that it lost frames, and what the state is.
func (h *eventsHandler) resync(ctx context.Context, reason events.ResyncReason) error {
	at := h.server.deps.Bus.Watermark()
	if err := h.writeJSON(ctx, events.Event{
		Time: time.Now().UTC(),
		Type: events.TypeResync,
		Data: map[string]any{"reason": reason.String()},
	}); err != nil {
		return err
	}
	return h.writeJSON(ctx, events.Event{
		Seq:  at,
		Time: time.Now().UTC(),
		Type: events.TypeSnapshot,
		Data: h.server.snapshot(h.scope(), at),
	})
}

// snapshotFrame is the snapshot as it goes on the wire.
//
// The bus layer's events.Snapshot carries what that leaf package can describe;
// pending approvals are added here, because this is the layer that holds both
// the broker and the serialisation of an approval, and duplicating that shape
// into the bus would be a second thing to keep in step.
type snapshotFrame struct {
	events.Snapshot
	Approvals []approvals.View `json:"approvals"`
}

// scope reports the sessions this connection asked for. An empty result means
// "everything", which is what a connection with no filter wants.
func (h *eventsHandler) scope() []string {
	h.subMu.RLock()
	defer h.subMu.RUnlock()
	if len(h.filters) == 0 {
		return nil
	}
	out := make([]string, 0, len(h.filters))
	for id := range h.filters {
		out = append(out, id)
	}
	return out
}

// readLoop consumes client control frames.
func (h *eventsHandler) readLoop(ctx context.Context) {
	for {
		var msg clientMessage
		if err := wsjson.Read(ctx, h.ws, &msg); err != nil {
			// Any read error ends the connection: the client is gone, or it sent
			// something that is not a control frame.
			_ = h.ws.Close(websocket.StatusNormalClosure, "")
			return
		}

		switch msg.Type {
		case "subscribe":
			if msg.SessionID == "" {
				continue
			}
			first := h.addFilter(msg.SessionID)
			// Subscribing is activity, so it keeps that session's lease alive.
			h.server.deps.Leases.Touch(msg.SessionID)
			// A session the client has just started watching is the one case
			// where it has no context at all: it asked for frames about a
			// session, and if a turn is already running there, the frames that
			// announced it are in the past. Sending the session's slice of the
			// snapshot closes that hole without a REST round trip.
			if first {
				if err := h.sessionSnapshot(ctx, msg.SessionID); err != nil {
					return
				}
			}

		case "unsubscribe":
			h.subMu.Lock()
			delete(h.filters, msg.SessionID)
			h.subMu.Unlock()

		case "ping":
			// Application-level ping, distinct from the WebSocket control frame.
			// A client uses it to keep a session lease warm without sending data.
			if err := h.ping(ctx); err != nil {
				return
			}
		}
	}
}

// addFilter records a subscription and reports whether it was new.
func (h *eventsHandler) addFilter(sessionID string) bool {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	if _, ok := h.filters[sessionID]; ok {
		return false
	}
	h.filters[sessionID] = struct{}{}
	return true
}

// sessionSnapshot sends the state of one session to a client that has just
// started watching it.
func (h *eventsHandler) sessionSnapshot(ctx context.Context, sessionID string) error {
	at := h.server.deps.Bus.Watermark()
	return h.writeJSON(ctx, events.Event{
		Seq:       at,
		Time:      time.Now().UTC(),
		Type:      events.TypeSnapshot,
		SessionID: sessionID,
		Data:      h.server.snapshot([]string{sessionID}, at),
	})
}

// match reports whether an event is in scope for this connection.
func (h *eventsHandler) match(e events.Event) bool {
	h.subMu.RLock()
	defer h.subMu.RUnlock()

	if len(h.filters) == 0 {
		return true
	}
	if e.SessionID == "" {
		// Gateway-wide events such as harness.state are always relevant: a client
		// that filtered to one session still needs to know the harness restarted.
		return true
	}
	_, ok := h.filters[e.SessionID]
	return ok
}

// writeJSON sends one frame, serialised against other writers.
func (h *eventsHandler) writeJSON(ctx context.Context, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	// A short write deadline keeps a stalled peer from blocking the pump forever.
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return h.ws.Write(writeCtx, websocket.MessageText, payload)
}

// ping sends a WebSocket ping control frame and waits for the pong.
func (h *eventsHandler) ping(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return h.ws.Ping(pingCtx)
}

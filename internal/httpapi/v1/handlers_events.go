package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/huanghantao/dsh-gateway/internal/app/events"
	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/httpcore"
)

// eventsHandler serves the multiplexed event stream.
//
// The connection is the client's window onto every session at once, so it must
// survive radio changes, backgrounded tabs, and sleeping phones. Three details
// make that work:
//
//   - Replay from a sequence number, so a reconnect does not lose output.
//   - Server-side pings, so an idle tunnel is kept open and a dead peer is
//     noticed.
//   - A resync signal when the client fell too far behind, so the UI refetches
//     instead of rendering a stream with an invisible hole in it.
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

	var since uint64
	if raw := r.URL.Query().Get("since"); raw != "" {
		var parsed uint64
		for i := 0; i < len(raw); i++ {
			if raw[i] < '0' || raw[i] > '9' {
				parsed = 0
				break
			}
			parsed = parsed*10 + uint64(raw[i]-'0')
		}
		since = parsed
	}

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
		"device", principal.DeviceID, "since", since, "client_ip", clientKey(r))
	defer func() {
		_ = ws.CloseNow()
		s.deps.Logger.Info("event stream closed", "device", principal.DeviceID)
	}()

	// A generous read limit: clients send only small control frames, so a large
	// frame means something is wrong.
	ws.SetReadLimit(64 << 10)

	sub, replayMissing := s.deps.Bus.Subscribe(since, h.match)
	defer sub.Close()

	// Bound the lifetime so that a forgotten tab does not hold a connection (and
	// a bus subscription) open indefinitely. Clients reconnect with `since`, so
	// this is invisible to them.
	// A bounded lifetime, not a derived deadline: the connection ends when its
	// own 12-hour budget expires or the client leaves. Clients reconnect with
	// `since`, so the bound is invisible to them.
	ctx, cancel := context.WithTimeout(h.ctx, 12*time.Hour)
	defer cancel()

	// Reading is handled in its own goroutine because the write path blocks on
	// the network; running them in sequence would mean a client that stops
	// reading also stops being able to send.
	go h.readLoop(ctx) //nolint:contextcheck // connected to the client, not to a request

	if err := h.writeJSON(ctx, map[string]any{ //nolint:contextcheck // see above
		"type": events.TypeHello,
		"data": map[string]any{
			"deviceId":     principal.DeviceID,
			"replayFrom":   since,
			"serverTime":   time.Now().UTC(),
			"harnessState": string(s.deps.Harness.State()),
		},
	}); err != nil {
		return
	}
	if replayMissing {
		// The client asked for events older than the replay buffer holds. Tell it
		// plainly so it refetches rather than rendering an incomplete stream.
		// Seq 0 marks a control frame that carries no position in the stream, so
		// a client must not bookmark it as its resume point.
		if err := h.writeJSON(ctx, events.Event{ //nolint:contextcheck // see above
			Time: time.Now().UTC(),
			Type: events.TypeResync,
			Data: map[string]any{"reason": "replay window exceeded"},
		}); err != nil {
			return
		}
	}

	h.pump(ctx, sub) //nolint:contextcheck // connected to the client, not to a request
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
			if sub.TakeResync() {
				if err := h.writeJSON(ctx, events.Event{
					Time: time.Now().UTC(),
					Type: events.TypeResync,
					Data: map[string]any{"reason": "this client fell behind and events were dropped"},
				}); err != nil {
					return
				}
			}
			if err := h.writeJSON(ctx, e); err != nil {
				return
			}
		}
	}
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
			h.subMu.Lock()
			h.filters[msg.SessionID] = struct{}{}
			h.subMu.Unlock()
			// Subscribing is activity, so it keeps that session's lease alive.
			h.server.deps.Leases.Touch(msg.SessionID)

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

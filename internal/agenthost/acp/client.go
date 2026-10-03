package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/huanghantao/dsh-gateway/internal/errx"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// conn is a bidirectional JSON-RPC 2.0 connection over newline-delimited JSON.
//
// Concurrency model:
//
//   - One writer goroutine owns w. Frames are queued on a channel and written one
//     at a time, so a frame is never interleaved with another. No caller touches
//     w directly, which makes half-written lines structurally impossible.
//   - One reader goroutine owns r. It dispatches responses to waiting callers,
//     hands notifications to the notification handler, and runs request handlers
//     on their own goroutines so that a handler which blocks — an approval
//     waiting for a human, for instance — cannot stall the stream.
//   - Callers block on a per-request channel and honour context cancellation.
type conn struct {
	r      io.Reader
	w      io.Writer
	logger *logx.Logger

	// writeCh carries encoded frames to the writer goroutine.
	writeCh chan []byte
	// pending maps a request id to the channel awaiting its response.
	pending sync.Map // string -> chan rpcMessage

	nextID atomic.Int64

	// onNotification handles a notification. It must not block.
	onNotification func(method string, params json.RawMessage)
	// onRequest handles a server-initiated request. It may block; the connection
	// runs it on its own goroutine.
	onRequest func(ctx context.Context, method string, params json.RawMessage) (any, error)

	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
	closeMu   sync.Mutex
}

func newConn(r io.Reader, w io.Writer, logger *logx.Logger) *conn {
	return &conn{
		r:       r,
		w:       w,
		logger:  logger,
		writeCh: make(chan []byte, 64),
		closed:  make(chan struct{}),
	}
}

// start launches the reader and writer goroutines.
func (c *conn) start() {
	go c.writeLoop()
	go c.readLoop()
}

// Done is closed when the connection has failed or been shut down.
func (c *conn) Done() <-chan struct{} { return c.closed }

// Err reports why the connection ended. It is only meaningful after Done closes.
func (c *conn) Err() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeErr
}

// close terminates the connection, failing every in-flight request.
func (c *conn) close(err error) {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closeErr = err
		c.closeMu.Unlock()
		close(c.closed)
	})
	// Unblock any caller parked on writeCh; the writer goroutine has exited.
	// Draining is unnecessary: sends select on c.closed.
}

// Call performs a request and decodes its result into out.
func (c *conn) Call(ctx context.Context, method string, params any, out any) error {
	if err := c.waitReady(ctx); err != nil {
		return err
	}

	rawParams, err := marshalParams(params)
	if err != nil {
		return err
	}

	id := c.nextID.Add(1)
	idRaw := json.RawMessage([]byte(fmt.Sprintf("%d", id)))

	frame, err := json.Marshal(rpcMessage{
		JSONRPC: "2.0",
		ID:      idRaw,
		Method:  method,
		Params:  rawParams,
	})
	if err != nil {
		return fmt.Errorf("acp: encode %s: %w", method, err)
	}

	// Register before sending: DSH can answer in the time it takes this
	// goroutine to reach the select, and a response with no waiter is dropped.
	ch := make(chan rpcMessage, 1)
	key := idKey(idRaw)
	c.pending.Store(key, ch)
	defer c.pending.Delete(key)

	if err := c.send(ctx, frame); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return errx.Wrap(ctx.Err(), errx.KindTimeout, "harness_timeout",
			fmt.Sprintf("the harness did not answer %s in time", method))
	case <-c.closed:
		return c.wrapClosed()
	case msg := <-ch:
		if msg.Error != nil {
			return c.mapRPCError(method, msg.Error)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(msg.Result, out); err != nil {
			return fmt.Errorf("acp: decode %s result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a notification, which by definition has no response.
func (c *conn) Notify(ctx context.Context, method string, params any) error {
	if err := c.waitReady(ctx); err != nil {
		return err
	}
	rawParams, err := marshalParams(params)
	if err != nil {
		return err
	}
	frame, err := json.Marshal(rpcMessage{JSONRPC: "2.0", Method: method, Params: rawParams})
	if err != nil {
		return fmt.Errorf("acp: encode %s: %w", method, err)
	}
	return c.send(ctx, frame)
}

// send queues one encoded frame for the writer goroutine.
func (c *conn) send(ctx context.Context, frame []byte) error {
	select {
	case c.writeCh <- frame:
		return nil
	case <-c.closed:
		return c.wrapClosed()
	case <-ctx.Done():
		return errx.Wrap(ctx.Err(), errx.KindTimeout, "harness_timeout", "the harness write queue is full")
	}
}

// waitReady reports whether the connection is still usable.
func (c *conn) waitReady(ctx context.Context) error {
	select {
	case <-c.closed:
		return c.wrapClosed()
	case <-ctx.Done():
		return errx.Wrap(ctx.Err(), errx.KindTimeout, "harness_timeout", "cancelled before the harness was called")
	default:
		return nil
	}
}

func (c *conn) wrapClosed() error {
	err := c.Err()
	if err == nil {
		err = errors.New("connection closed")
	}
	return errx.Wrap(err, errx.KindUnavailable, "harness_unavailable",
		"the harness process is not available")
}

// mapRPCError converts a JSON-RPC error into the gateway's taxonomy.
//
// DSH reports prompt failures — a missing provider credential, for example — as
// a -32603 with a genuinely useful message. That message is surfaced to the
// operator rather than hidden: this is a single-operator tool, the credential is
// the operator's own, and "no API key for provider route X" is exactly what they
// need to read. Nothing here is derived from another user's data.
func (c *conn) mapRPCError(method string, e *rpcError) error {
	switch e.Code {
	case methodNotFound:
		return errx.New(errx.KindUnavailable, "harness_unsupported",
			fmt.Sprintf("the harness does not implement %s", method))
	case -32602: // invalid params
		return errx.New(errx.KindInvalid, "harness_invalid_request", e.Message)
	default:
		return errx.New(errx.KindUnavailable, "harness_error", e.Message)
	}
}

// writeLoop is the sole writer of c.w.
func (c *conn) writeLoop() {
	for {
		select {
		case <-c.closed:
			return
		case frame := <-c.writeCh:
			// A newline terminates the frame; ACP uses newline-delimited JSON,
			// not Content-Length framing.
			buf := make([]byte, 0, len(frame)+1)
			buf = append(buf, frame...)
			buf = append(buf, '\n')
			if _, err := c.w.Write(buf); err != nil {
				c.logger.Debug("acp write failed", "error", err.Error())
				c.close(fmt.Errorf("acp: write frame: %w", err))
				return
			}
		}
	}
}

// readLoop is the sole reader of c.r.
func (c *conn) readLoop() {
	scanner := bufio.NewScanner(c.r)
	// Committed assistant messages can be large; 16 MiB is far above any real
	// frame and still bounds a desynchronised stream.
	scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue // ACP permits blank lines between frames
		}
		// scanner.Bytes() is only valid until the next Scan, and dispatch may
		// hand the slice to another goroutine, so take a copy.
		frame := make([]byte, len(line))
		copy(frame, line)

		msg, err := decodeFrame(frame)
		if err != nil {
			c.logger.Warn("acp frame rejected", "error", err.Error())
			continue
		}
		c.dispatch(msg)
	}

	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	c.logger.Debug("acp reader stopped", "error", err.Error())
	c.close(fmt.Errorf("acp: read frame: %w", err))
}

// dispatch routes one decoded message.
func (c *conn) dispatch(msg rpcMessage) {
	switch {
	case msg.isResponse():
		key := idKey(msg.ID)
		if waiter, ok := c.pending.Load(key); ok {
			if ch, isChan := waiter.(chan rpcMessage); isChan {
				// Buffered with capacity 1, and each id is used once, so this
				// never blocks even if the caller already gave up on its context.
				ch <- msg
			}
		} else {
			// A response to a request we abandoned via context cancellation.
			c.logger.Debug("acp response for an abandoned request", "id", key)
		}

	case msg.isNotification():
		if c.onNotification != nil {
			// Contract: the handler must not block. A channel send with a
			// default case, or a lock-free publish, satisfies this.
			c.onNotification(msg.Method, msg.Params)
		}

	case msg.isRequest():
		if c.onRequest == nil {
			c.respondError(msg.ID, methodNotFound, "no handler for "+msg.Method)
			return
		}
		// Run on its own goroutine: an approval handler legitimately blocks for
		// minutes, and must not stall updates for every other session.
		go func(m rpcMessage) {
			result, err := c.onRequest(context.Background(), m.Method, m.Params)
			if err != nil {
				var e *errx.Error
				if errors.As(err, &e) && e.Kind == errx.KindInvalid {
					c.respondError(m.ID, -32602, e.Msg)
					return
				}
				c.respondError(m.ID, -32603, err.Error())
				return
			}
			c.respond(m.ID, result)
		}(msg)

	default:
		c.logger.Warn("acp message had neither method nor id")
	}
}

// respond writes a successful response to a server-initiated request.
func (c *conn) respond(id json.RawMessage, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		c.respondError(id, -32603, "failed to encode result")
		return
	}
	frame, err := json.Marshal(rpcMessage{JSONRPC: "2.0", ID: id, Result: raw})
	if err != nil {
		return
	}
	// A response must go out even while shutting down, so bypass the context and
	// rely on the writer goroutine still running. If it has exited the send is
	// dropped, which is correct: the process is gone.
	select {
	case c.writeCh <- frame:
	case <-c.closed:
	}
}

// respondError writes a JSON-RPC error response.
func (c *conn) respondError(id json.RawMessage, code int, message string) {
	frame, err := json.Marshal(rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: message},
	})
	if err != nil {
		return
	}
	select {
	case c.writeCh <- frame:
	case <-c.closed:
	}
}

// marshalParams encodes params, treating nil as an empty object: DSH's handlers
// expect a params member to be present.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return json.RawMessage("{}"), nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("acp: encode params: %w", err)
	}
	return raw, nil
}

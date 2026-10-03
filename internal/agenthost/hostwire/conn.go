// This file is the transport: one connection, one reader, one writer, and the
// correlation between a request and its response.
//
// It is deliberately small and knows nothing about agents. Everything above it —
// sessions, turns, approvals — is expressed in terms of Call and Handle, which
// is what makes the protocol testable without a harness and the harness testable
// without a socket.
package hostwire

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// maxFrame bounds one protocol message. The largest thing on this wire is a
// prompt with an image, base64-encoded into JSON, so the limit is generous
// relative to that and small relative to "an unbounded read from a pipe".
const maxFrame = 16 << 20

// ErrClosed is returned to callers whose request was in flight when the
// connection ended. It is exported because the layer above distinguishes "the
// host went away and I should reconnect" from "the host refused this".
var ErrClosed = errors.New("hostwire: connection closed")

// Handler answers either a notification or a request, and knows which by
// whether the Request it was given carries an ID.
//
// One type covers both because the two differ in exactly one respect — whether
// there is somewhere to put the answer — and modelling that as two registration
// tables would mean a method could be registered as the wrong kind and fail at
// runtime instead of at the call site.
type Handler func(r *Request) error

// Request is one inbound message that expects handling.
//
// A handler answers with Reply. The transport also answers on the handler's
// behalf if it returns an error without having replied, so a handler that forgets
// produces a failed response rather than a caller that waits forever.
//
// That fallback is not politeness, it is the fix for a real defect: a handler
// that returned a refusal by mistake, without replying, left the peer blocked
// until its own timeout — a two-minute hang whose cause was a missing line in a
// different function. A protocol where "no answer" is representable will
// eventually represent it by accident, so the transport makes it unrepresentable.
type Request struct {
	// Method is the command or notification name.
	Method string
	// Params is the argument as it arrived.
	Params json.RawMessage

	// ctx ends when the connection does.
	ctx context.Context

	reply    func(result any, failure *Error) error
	answered atomic.Bool
}

// Notification reports whether this message has no answer.
func (r *Request) Notification() bool { return r.reply == nil }

// Context is the connection's lifetime. A handler that waits — and
// `turn.await` and `permission.request` both do — should stop waiting when the
// caller has gone, because nobody is left to hear the answer.
func (r *Request) Context() context.Context { return r.ctx }

// Decode unmarshals the params into v.
func (r *Request) Decode(v any) error {
	if len(r.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return fmt.Errorf("hostwire: decode %s params: %w", r.Method, err)
	}
	return nil
}

// Reply answers a request. It is a no-op on a notification, so a handler does
// not have to know which kind it was given to write one answer path.
//
// A second Reply is also a no-op: the peer correlates by id and would treat a
// duplicate as an answer to a request it no longer has.
func (r *Request) Reply(result any, err error) error {
	if r.reply == nil || r.answered.Swap(true) {
		return nil
	}
	return r.reply(result, ErrorOf(err))
}

// answeredBy reports whether the handler replied. The transport uses it to
// decide whether an error still needs sending.
func (r *Request) answeredBy() bool { return r.answered.Load() }

// Conn is one connection to the agent host, from either end.
type Conn struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	writeMu sync.Mutex
	nextID  atomic.Uint64

	mu       sync.Mutex
	pending  map[string]chan response
	handlers map[string]Handler
	closed   chan struct{}
	closeErr error
	// connCtx ends with the connection, so a handler that is waiting on a
	// long-running answer can stop when nobody is left to hear it.
	connCtx    context.Context
	connCancel context.CancelFunc

	// onClose runs once when the connection ends, so an owner can react to a
	// host that went away without polling.
	onClose func(error)

	// started guards Start, so a connection's reader is launched exactly once.
	started sync.Once

	// classify renders an error the handler returned as a wire failure. It is
	// supplied by the owning side, because only that side knows what its errors
	// mean; this package deliberately does not.
	classify func(error) *Error
}

// SetErrorClassifier installs the mapping from this end's errors to wire ones.
// Without it, an error a handler returns is reported as internal — correct, and
// less useful than it could be.
func (c *Conn) SetErrorClassifier(fn func(error) *Error) {
	c.mu.Lock()
	c.classify = fn
	c.mu.Unlock()
}

// classifyError applies the installed classifier, or the safe default.
func (c *Conn) classifyError(err error) *Error {
	c.mu.Lock()
	fn := c.classify
	c.mu.Unlock()
	if fn != nil {
		return fn(err)
	}
	return ErrorOf(err)
}

type response struct {
	result json.RawMessage
	err    *Error
}

// NewConn wraps an established connection. Its reader is not started until
// Start is called.
//
// The two-step construction is not ceremony: both ends must register their
// handlers *before* the first frame is dispatched, because the first frame is
// the handshake. A reader that starts inside the constructor can deliver a
// `hello` to a handler table that is still being filled, which shows up as
// "unknown method" on a connection that is in fact perfectly healthy.
func NewConn(c net.Conn) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	conn := &Conn{
		connCtx:    ctx,
		connCancel: cancel,
		conn:       c,
		r:          bufio.NewReaderSize(c, 64<<10),
		w:          bufio.NewWriterSize(c, 64<<10),
		pending:    map[string]chan response{},
		handlers:   map[string]Handler{},
		closed:     make(chan struct{}),
	}
	return conn
}

// Start launches the reader. It is idempotent.
func (c *Conn) Start() {
	c.started.Do(func() { go c.read() })
}

// Done is closed when the connection ends, for any reason.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Err reports why the connection ended, and is nil while it is open.
func (c *Conn) Err() error {
	select {
	case <-c.closed:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.closeErr
	default:
		return nil
	}
}

// OnClose registers a callback for the end of the connection. It fires exactly
// once, including when the connection is already closed.
func (c *Conn) OnClose(fn func(error)) {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		fn(c.Err())
		return
	default:
	}
	c.onClose = fn
	c.mu.Unlock()
}

// Handle registers the handler for a notification name.
func (c *Conn) Handle(event string, h Handler) {
	c.mu.Lock()
	c.handlers[event] = h
	c.mu.Unlock()
}

// Call sends a command and waits for its response.
//
// A zero deadline means no deadline. That is not laziness: `turn.await` is
// *supposed* to block for as long as the turn runs, and a transport that
// imposed its own timeout would turn a long turn into a failure.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	deadline, hasDeadline := ctx.Deadline()

	raw, err := paramsOrNil(params)
	if err != nil {
		return err
	}
	id := strconv.FormatUint(c.nextID.Add(1), 10)

	ch := make(chan response, 1)
	c.mu.Lock()
	select {
	case <-c.closed:
		err := c.closeErr
		c.mu.Unlock()
		return err
	default:
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(Frame{ID: id, Method: method, Params: raw}); err != nil {
		c.forget(id)
		return err
	}

	var wait <-chan time.Time
	if hasDeadline {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		wait = timer.C
	}

	select {
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		if result == nil || len(resp.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.result, result); err != nil {
			return fmt.Errorf("hostwire: decode %s result: %w", method, err)
		}
		return nil
	case <-wait:
		c.forget(id)
		return fmt.Errorf("hostwire: %s: %w", method, context.DeadlineExceeded)
	case <-c.closed:
		c.forget(id)
		return c.Err()
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	}
}

// Notify sends a notification. It never waits for an answer, because there is
// none.
func (c *Conn) Notify(event string, data any) error {
	raw, err := paramsOrNil(data)
	if err != nil {
		return err
	}
	return c.write(Frame{Event: event, Data: raw})
}

// Reply answers a request that arrived on this connection.
func (c *Conn) Reply(id string, result any, failure *Error) error {
	raw, err := paramsOrNil(result)
	if err != nil {
		return err
	}
	return c.write(Frame{ID: id, Result: raw, Error: failure})
}

// Close ends the connection. It is safe to call more than once.
func (c *Conn) Close() error {
	c.shutdown(ErrClosed)
	return c.conn.Close()
}

/* ------------------------------------------------------------------ internals */

func paramsOrNil(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("hostwire: encode params: %w", err)
	}
	return raw, nil
}

func (c *Conn) write(f Frame) error {
	payload, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("hostwire: encode frame: %w", err)
	}
	if len(payload) > maxFrame {
		return fmt.Errorf("hostwire: frame of %d bytes exceeds the %d limit", len(payload), maxFrame)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.closed:
		return c.Err()
	default:
	}

	if _, err := c.w.Write(payload); err != nil {
		c.shutdown(err)
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		c.shutdown(err)
		return err
	}
	// Flushed per frame rather than batched. A protocol whose whole purpose is
	// that one side can disappear at any moment cannot hold a message in a
	// buffer waiting for company.
	if err := c.w.Flush(); err != nil {
		c.shutdown(err)
		return err
	}
	return nil
}

// read is the connection's single reader. Everything that arrives is handled
// here, in order, so no other goroutine has to think about framing.
func (c *Conn) read() {
	for {
		line, err := readLine(c.r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.shutdown(ErrClosed)
			} else {
				c.shutdown(err)
			}
			return
		}
		if len(line) == 0 {
			continue
		}
		var f Frame
		if err := json.Unmarshal(line, &f); err != nil {
			// A malformed frame is not recoverable: the stream has lost its
			// alignment, and guessing where the next message starts is how a
			// protocol bug becomes a security bug.
			c.shutdown(fmt.Errorf("hostwire: malformed frame: %w", err))
			return
		}
		c.dispatch(f)
	}
}

func (c *Conn) dispatch(f Frame) {
	switch {
	case f.Event != "":
		c.mu.Lock()
		h := c.handlers[f.Event]
		c.mu.Unlock()
		if h == nil {
			return // an unknown notification is ignored: forward compatibility
		}
		// Handled on its own goroutine for the same reason a request is: a
		// handler that blocks must not stop the reader behind it.
		go func() {
			if err := h(&Request{Method: f.Event, Params: f.Data, ctx: c.connCtx}); err != nil {
				// There is no reply to a notification, so the only honest place
				// to put a failure is the log on the far side.
				_ = c.Notify(MethodEvent, Error{
					Kind:    KindInternal,
					Code:    "notification_failed",
					Message: err.Error(),
				})
			}
		}()

	case f.Method != "":
		// A request. The other end is the host in both directions of this
		// protocol, so this branch is how `permission.request` reaches a
		// gateway and how the gateway asks for anything.
		c.mu.Lock()
		h := c.handlers[f.Method]
		c.mu.Unlock()
		if h == nil {
			_ = c.Reply(f.ID, nil, &Error{
				Kind:    KindInvalid,
				Code:    "unknown_method",
				Message: "this end does not implement " + f.Method,
			})
			return
		}
		// Handled on its own goroutine: a handler that blocks — and
		// `permission.request` blocks on a human — must not stop the reader,
		// because the response to somebody else's request is behind it in the
		// stream.
		go func() {
			req := &Request{
				Method: f.Method,
				Params: f.Params,
				ctx:    c.connCtx,
				reply: func(result any, failure *Error) error {
					return c.Reply(f.ID, result, failure)
				},
			}
			err := h(req)
			// A handler that returned a failure without answering has still
			// failed, and the peer is still waiting. Saying so is the difference
			// between a refusal and a hang.
			if err != nil && !req.answeredBy() {
				_ = req.reply(nil, c.classifyError(err))
			}
		}()

	default:
		// A response.
		c.mu.Lock()
		ch := c.pending[f.ID]
		delete(c.pending, f.ID)
		c.mu.Unlock()
		if ch == nil {
			return // the caller gave up; the answer is not interesting
		}
		ch <- response{result: f.Result, err: f.Error}
	}
}

func (c *Conn) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// shutdown ends the connection once, failing everything still in flight.
func (c *Conn) shutdown(cause error) {
	c.mu.Lock()
	select {
	case <-c.closed:
		c.mu.Unlock()
		return
	default:
	}
	if cause == nil {
		cause = ErrClosed
	}
	if c.connCancel != nil {
		c.connCancel()
	}
	c.closeErr = cause
	pending := c.pending
	c.pending = map[string]chan response{}
	onClose := c.onClose
	close(c.closed)
	c.mu.Unlock()

	for _, ch := range pending {
		// Buffered, so this cannot block even if the waiter has already gone.
		ch <- response{err: &Error{
			Kind:    KindUnavailable,
			Code:    "connection_lost",
			Message: cause.Error(),
		}}
	}
	if onClose != nil {
		onClose(cause)
	}
}

// readLine reads one newline-terminated frame, bounded by maxFrame.
//
// bufio.Reader has no bounded ReadString, so the bound is imposed here rather
// than trusted to the peer. A frame that exceeds it ends the connection: a
// stream that has run past the limit has no recoverable position.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxFrame {
			return nil, fmt.Errorf("hostwire: frame exceeds %d bytes", maxFrame)
		}
		if err == nil {
			// Trim the newline; keep everything else.
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

// ErrorOf renders an error as a wire value.
//
// Two shapes reach here, and the second is the one that matters:
//
//   - a wire error, already shaped: passed through untouched;
//   - anything else, most often an unclassified failure: internal, keeping its
//     message for the log.
//
// An error that carries its own classification is rendered by ClassifyError
// instead. The split is deliberate. This package is the wire contract between
// two processes and must not depend on either side's error vocabulary, so it
// cannot know what a "kind" is; the layer that *does* know — the anticorruption
// layer that already translates every other type across the seam — is where the
// mapping belongs. What this function guarantees is the fallback: an error that
// nobody classified is never presented to a caller as something to retry.
func ErrorOf(err error) *Error {
	if err == nil {
		return nil
	}
	var wire *Error
	if errors.As(err, &wire) {
		return wire
	}
	return &Error{
		Kind:    KindInternal,
		Code:    "internal",
		Message: err.Error(),
	}
}

// ClassifyError renders a classified failure, preserving the kind and code.
//
// It exists so the anticorruption layer can say what an application error *is*
// without this package having to know: the caller supplies the kind and code it
// already understands, and the message comes from the error itself.
func ClassifyError(err error, kind, code string) *Error {
	if err == nil {
		return nil
	}
	if code == "" {
		code = "internal"
	}
	return &Error{
		Kind:      kind,
		Code:      code,
		Message:   err.Error(),
		Retryable: kind == KindRateLimited || kind == KindUnavailable || kind == KindTimeout,
	}
}

// Error implements error so a wire error travels as an ordinary Go error.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

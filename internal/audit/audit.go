// Package audit records security-relevant events to an append-only log.
//
// The log is deliberately boring: newline-delimited JSON, one file, rotated by
// size. It exists so that an operator can answer "what happened?" after the
// fact — which device paired, when a turn was approved, who cancelled what. It
// is not a metrics system and is never read on the request path.
//
// Every record carries the request id, so an audit line can be correlated with
// the access log without duplicating request detail.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/logx"
	"github.com/huanghantao/dsh-gateway/internal/reqctx"
)

// Event names the classes of activity worth recording.
type Event string

const (
	// EventPaired records a successful device enrollment.
	EventPaired Event = "device.paired"
	// EventPairFailed records a rejected pairing attempt.
	EventPairFailed Event = "device.pair_failed"
	// EventRevoked records a device being disabled.
	EventRevoked Event = "device.revoked"
	// EventAuthFailed records a rejected credential.
	EventAuthFailed Event = "auth.failed"
	// EventSessionOpened records a session lease being acquired.
	EventSessionOpened Event = "session.opened"
	// EventSessionReleased records a lease ending and the DSH lock being freed.
	EventSessionReleased Event = "session.released"
	// EventPromptSent records a prompt being admitted.
	EventPromptSent Event = "prompt.sent"
	// EventPromptCancelled records an operator-requested cancellation.
	EventPromptCancelled Event = "prompt.cancelled"
	// EventApprovalDecided records an allow/reject decision. This is the single
	// most important record in the log: it is the trace of a human authorising
	// a command to run on their machine.
	EventApprovalDecided Event = "approval.decided"
	// EventApprovalGrantRevoked records a standing authorisation being withdrawn.
	//
	// A grant is the one way a tool can run without a person answering *this*
	// prompt, so both ends of it are audited: the decision that created it is an
	// EventApprovalDecided carrying a scoped option, and this is the record that
	// it stopped applying.
	EventApprovalGrantRevoked Event = "approval.grant_revoked"
	// EventWorkspaceReverted records an undo writing to the operator's files.
	//
	// It is the only audit event describing a change this gateway made to a
	// workspace rather than one it observed, which makes it the record to read
	// when a file is not what someone left it as.
	EventWorkspaceReverted Event = "workspace.reverted"
	// EventHarnessRestarted records the supervised DSH process restarting.
	EventHarnessRestarted Event = "harness.restarted"
	// EventHarnessFailed records a harness fault the operator may need to act on.
	EventHarnessFailed Event = "harness.failed"
)

// Record is one audit line.
type Record struct {
	Time      time.Time      `json:"time"`
	Event     Event          `json:"event"`
	RequestID string         `json:"requestId,omitempty"`
	DeviceID  string         `json:"deviceId,omitempty"`
	ClientIP  string         `json:"clientIp,omitempty"`
	Subject   string         `json:"subject,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Logger appends records to a rotating file.
type Logger struct {
	path     string
	maxBytes int64
	logger   *logx.Logger
	now      func() time.Time

	mu      sync.Mutex
	file    *os.File
	written int64
}

// Config parameterises the audit logger.
type Config struct {
	// Path is the log file. Its directory is created if missing.
	Path string
	// MaxBytes triggers a single-generation rotation (.1 suffix).
	MaxBytes int64
}

// Open prepares the audit log.
func Open(cfg Config, logger *logx.Logger, now func() time.Time) (*Logger, error) {
	if now == nil {
		now = time.Now
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 8 << 20 // 8 MiB
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", filepath.Dir(cfg.Path), err)
	}
	l := &Logger{path: cfg.Path, maxBytes: cfg.MaxBytes, logger: logger, now: now}
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	return l, nil
}

// Write appends rec. A failure to audit must never fail the request it
// describes, so the error is surfaced but callers are expected to log and
// continue; the alternative — rejecting a legitimate approval because the disk
// is full — would be worse.
func (l *Logger) Write(rec Record) error {
	if rec.Time.IsZero() {
		rec.Time = l.now()
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: encode record: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.written+int64(len(line)) > l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.file.Write(line)
	l.written += int64(n)
	if err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	return nil
}

// Record builds and writes one record, deriving the request id from ctx.
func (l *Logger) Record(ctx context.Context, event Event, subject string, fields map[string]any) {
	rec := Record{
		Event:     event,
		RequestID: reqctx.ID(ctx),
		Subject:   subject,
		Fields:    fields,
	}
	if err := l.Write(rec); err != nil && l.logger != nil {
		l.logger.ErrorContext(ctx, "audit write failed", "event", string(event), "error", err.Error())
	}
}

// Close flushes and closes the log.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *Logger) openLocked() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("audit: open %s: %w", l.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("audit: stat %s: %w", l.path, err)
	}
	l.file = f
	l.written = info.Size()
	return nil
}

// rotateLocked moves the current file aside and starts a fresh one. A single
// generation is kept: the audit log is a diagnostic aid, not a compliance
// archive, and unbounded growth on a personal machine is the worse failure.
func (l *Logger) rotateLocked() error {
	if l.file != nil {
		if err := l.file.Close(); err != nil {
			return fmt.Errorf("audit: close before rotate: %w", err)
		}
		l.file = nil
	}
	// Rename is atomic; a reader sees either the old or the new file.
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("audit: rotate: %w", err)
	}
	return l.openLocked()
}

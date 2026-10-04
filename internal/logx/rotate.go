package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultMaxBytes is the size a log reaches before it is rotated. It matches the
// audit log's bound, because both are the same kind of thing: a diagnostic aid
// on a personal machine.
const DefaultMaxBytes int64 = 16 << 20

// RotatingFile is an io.Writer that appends to a file and, once the file passes
// its size bound, moves it aside and starts a new one.
//
// It exists because the gateway under launchd does not own its own output:
// StandardErrorPath is a file launchd appends to for as long as the job exists,
// and macOS has no user-level rotator to hand it to (newsyslog needs root, which
// this install deliberately never asks for). A gateway that logs one line per
// HTTP request at info level therefore grows that file for as long as it runs,
// and the only thing that ever removes a byte is a human with rm.
//
// One generation is kept, and the previous one is overwritten — the same rule the
// audit log uses (internal/audit), for the same reason. The bound is therefore
// two files of maxBytes, not one.
type RotatingFile struct {
	path     string
	maxBytes int64

	mu      sync.Mutex
	file    *os.File
	written int64
	closed  bool
}

// OpenRotatingFile opens path for appending, creating the file and its directory
// if they do not exist. maxBytes <= 0 takes DefaultMaxBytes.
func OpenRotatingFile(path string, maxBytes int64) (*RotatingFile, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("logx: create %s: %w", dir, err)
	}
	r := &RotatingFile{path: path, maxBytes: maxBytes}
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// Write implements io.Writer.
//
// Rotation happens before the write rather than after it: a record is what a
// reader came for, and splitting one across two files would be a worse failure
// than a file that ends a record over its bound.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return 0, os.ErrClosed
	}
	if r.file == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	if r.written > 0 && r.written+int64(len(p)) > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := r.file.Write(p)
	r.written += int64(n)
	return n, err
}

// Close implements io.Closer. Closing twice is not an error.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// Path is where the log is written, so a caller can say so out loud.
func (r *RotatingFile) Path() string { return r.path }

// openLocked opens the file for appending and learns how much is already in it.
//
// The size is taken from the file rather than from a counter, so a restart
// continues from what the last process left instead of believing the log is
// empty and letting it grow to twice the bound.
func (r *RotatingFile) openLocked() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("logx: open %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logx: stat %s: %w", r.path, err)
	}
	r.file = f
	r.written = info.Size()
	return nil
}

// rotateLocked moves the current file aside and starts a fresh one. The caller
// holds r.mu.
func (r *RotatingFile) rotateLocked() error {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			return fmt.Errorf("logx: close before rotate: %w", err)
		}
		r.file = nil
	}
	// Rename is atomic, so a reader that has the old file open keeps reading it
	// rather than seeing a truncated one. Overwriting the previous generation is
	// what keeps the bound at two files.
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("logx: rotate %s: %w", r.path, err)
	}
	return r.openLocked()
}

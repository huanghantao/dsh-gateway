// Package atomicfile writes files so that a reader never observes a partially
// written or truncated document, and so that a crash mid-write leaves the
// previous contents intact.
//
// The sequence is the standard durable-replace dance: write to a temporary file
// in the destination directory, fsync it, rename it over the target, then fsync
// the directory. The temporary file must live in the same directory as the
// target because rename(2) is only atomic within a single filesystem.
package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFile durably replaces path with data.
//
// perm is applied to the final file; the temporary file is created 0600 and the
// permission is widened only at rename time, so secrets are never briefly
// world-readable.
func WriteFile(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomicfile: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Best-effort cleanup on every failure path.
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync temp: %w", err)
	}
	if err = tmp.Chmod(perm); err != nil {
		return fmt.Errorf("atomicfile: chmod temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("atomicfile: close temp: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomicfile: rename into place: %w", err)
	}
	return syncDir(dir)
}

// WriteFileSecret is WriteFile with 0600 permissions.
func WriteFileSecret(path string, data []byte) error {
	return WriteFile(path, data, 0o600)
}

// syncDir fsyncs a directory so the rename itself is durable. Some filesystems
// and platforms refuse to open a directory for reading; that is not fatal
// because the data is already in place, so the error is swallowed.
func syncDir(dir string) error {
	// dir comes from the caller's configuration, never from a request.
	d, err := os.Open(dir) //nolint:gosec // configuration-derived path
	if err != nil {
		return nil //nolint:nilerr // durability best-effort; the rename already succeeded
	}
	// A directory that cannot be closed is not worth failing a completed write
	// over; the rename that mattered has already succeeded.
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return nil //nolint:nilerr // see above
	}
	return nil
}

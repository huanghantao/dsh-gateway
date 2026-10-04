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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	syncDir(dir)
	return nil
}

// WriteFileSecret is WriteFile with 0600 permissions.
func WriteFileSecret(path string, data []byte) error {
	return WriteFile(path, data, 0o600)
}

// staleTempAge is how long a temporary file must have sat untouched before the
// sweep will touch it.
//
// An hour is far longer than any write here takes — these are the gateway's own
// small documents — and far shorter than forever. It exists because the sweep
// must not be able to delete a temp file that a live writer is still filling:
// `dsh-gateway pair` runs beside a running gateway and writes the same
// directory, and racing it would be a worse bug than the litter.
const staleTempAge = time.Hour

// SweepTemp removes temporary files this package left behind in dir, and reports
// how many went.
//
// WriteFile removes its own temporary on every failure it can observe, so what
// is left is a write the process did not survive: a SIGKILL, a power cut, a
// redeploy that ran out of patience. Each is silent, never read again, and as
// large as the document that was being written — so somebody has to look.
func SweepTemp(dir string, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("atomicfile: read %s: %w", dir, err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !isTempName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// It vanished between the read and the stat, which is the outcome
			// this sweep wanted anyway.
			continue
		}
		if now.Sub(info.ModTime()) < staleTempAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// isTempName reports whether name is the shape CreateTemp produces here:
// ".<document>.tmp-<random>". The leading dot keeps the litter out of listings,
// and is also what makes it distinguishable from a file anyone meant to keep.
func isTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-")
}

// syncDir fsyncs a directory so the rename itself is durable. Some filesystems
// and platforms refuse to open a directory for reading; that is not fatal
// because the data is already in place, so every failure here is swallowed.
func syncDir(dir string) {
	// dir comes from the caller's configuration, never from a request.
	d, err := os.Open(dir) //nolint:gosec // configuration-derived path
	if err != nil {
		return
	}
	// A directory that cannot be closed or synced is not worth failing a
	// completed write over; the rename that mattered has already succeeded.
	defer func() { _ = d.Close() }()
	_ = d.Sync()
}

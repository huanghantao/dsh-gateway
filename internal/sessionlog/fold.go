package sessionlog

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/errx"
)

// This file is the reading half of the store: it turns a log that grows one
// zstd frame per flush into the projection a transcript is rendered from, and it
// does so by reading only what was appended since the last read.
//
// Three facts about the container make that possible, and all three are
// properties of how DSH writes rather than of anything it promises:
//
//   - A log is append-only, so a byte offset that was the end of the file is
//     still a boundary later.
//   - A log is a concatenation of independently decodable zstd frames, one per
//     flush, so a boundary can be resumed from without reading what precedes it.
//   - A byte offset is only a boundary if the read that produced it decoded
//     cleanly to the end of the file, which is the one thing the reader can
//     check for itself and the reason a flush in progress is never mistaken for
//     a boundary.
//
// Nothing here parses the frame format. A resume point is remembered as "the
// file was this long when everything decoded", and the only fact taken from the
// container is the four-byte magic that says a frame starts where the last read
// stopped.

// errRewritten reports that a log is no longer the file a resume point was taken
// from: it was compacted, replaced, or written from somewhere other than its
// end. A reader that sees it can only start over.
var errRewritten = errors.New("sessionlog: the log was rewritten")

// progress is where a fold stopped in a log.
type progress struct {
	// committed is the offset whose frames have been decoded and fed to a parser
	// in full. Reading resumes here.
	committed int64
	// fed is how much of the decoded text after committed has already reached the
	// parser. It is non-zero only while the tail of the file cannot be consumed
	// yet — a flush in progress, or a line that is not finished — and it is what
	// keeps the retry from feeding the same lines twice. The retry decodes those
	// bytes again even though it does not fold them, which is what bounds the
	// region that matters: the frames written since the last read that ended on a
	// boundary, not the file.
	fed int64
}

// empty reports whether a read starting here begins at the beginning of a log
// and has consumed none of it.
func (p progress) empty() bool { return p.committed == 0 && p.fed == 0 }

// readFrom decodes the frames a log has grown by since p, handing every complete
// line to feed, and returns where the next read resumes.
//
// A tail that cannot be decoded yet — the frame the writer is in the middle of,
// or a line it has not finished — is not an error. The frames before it are fed,
// the returned progress stays behind the tail, and the next read picks it up
// once it is complete, which is what lets a reader watch a session that is being
// written instead of failing on it once a second.
//
// errRewritten is the one error it reports, and only for a resume point that is
// no longer a boundary of this file: the caller can answer that by folding from
// the beginning.
func (s *Store) readFrom(path string, p progress, feed func(line []byte)) (progress, error) {
	file, err := os.Open(path) //nolint:gosec // a path resolved and validated by logPath
	if err != nil {
		return p, errx.Wrap(err, errx.KindInternal, "transcript_unreadable", "")
	}
	// The file is read-only and already fully consumed by the time this runs; a
	// Close error on it is not actionable and must not mask a read error.
	defer func() { _ = file.Close() }()

	if p.committed > 0 {
		if _, err := file.Seek(p.committed, io.SeekStart); err != nil {
			return p, errx.Wrap(err, errx.KindInternal, "transcript_unreadable", "")
		}
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return p, errx.Wrap(err, errx.KindInternal, "transcript_unreadable", "")
	}
	if len(raw) == 0 {
		// Nothing was appended. An empty log is a session with no history yet,
		// not a failure: the parser is simply fed nothing.
		return p, nil
	}
	s.decoded.Add(uint64(len(raw)))

	// A resume point is a claim about the file's past, and the frame a flush
	// begins with is what can confirm it: a rewrite moves every boundary after
	// the byte it changed, so the claim fails loudly here rather than quietly
	// decoding nothing forever.
	if p.committed > 0 && !frameStart(raw) {
		return p, errRewritten
	}

	// DecodeAll decodes every whole frame it is given and reports the tail it
	// could not finish as an error, so a flush caught mid-write costs the frames
	// before it and nothing else.
	plain, decodeErr := s.dec.DecodeAll(raw, nil)

	next := p
	if unfed := int64(len(plain)) - p.fed; unfed > 0 {
		// Only whole lines reach the parser: a frame boundary is a flush
		// boundary, not necessarily a line boundary, and half an event must
		// never be folded into the transcript.
		text := plain[p.fed:]
		if end := bytes.LastIndexByte(text, '\n'); end >= 0 {
			feedLines(text[:end+1], feed)
			next.fed = p.fed + int64(end+1)
		}
	}

	if decodeErr == nil && next.fed == int64(len(plain)) {
		// Everything on disk decoded and every byte of it reached the parser, so
		// the end of the file is a boundary on both counts and the next read can
		// start there.
		next = progress{committed: p.committed + int64(len(raw))}
	}
	if decodeErr != nil {
		return next, errx.Wrap(decodeErr, errx.KindInternal, "transcript_undecodable",
			"the stored transcript could not be decompressed")
	}
	return next, nil
}

// feedLines hands each whole line in text to feed, skipping blank ones.
//
// text must end on a newline; the caller holds back an unfinished line rather
// than fold half an event.
func feedLines(text []byte, feed func(line []byte)) {
	for len(text) > 0 {
		end := bytes.IndexByte(text, '\n')
		if end < 0 {
			return
		}
		if line := bytes.TrimSpace(text[:end]); len(line) > 0 {
			feed(line)
		}
		text = text[end+1:]
	}
}

// frameStart reports whether raw begins a zstd frame — the standard one, or one
// of the skippable frames a stream may carry.
//
// This is the only thing the store knows about the container, and it is
// deliberately about the container rather than about the decoded text: a resume
// point that lands inside a frame decodes to nothing, which is indistinguishable
// from a flush that never finishes.
func frameStart(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	if raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd {
		return true
	}
	// Skippable frames, little-endian magic 0x184D2A50 through 0x184D2A5F.
	return raw[0] >= 0x50 && raw[0] <= 0x5f && raw[1] == 0x2a && raw[2] == 0x4d && raw[3] == 0x18
}

// stale reports whether a fold that consumed a file of this size and mtime is
// looking at a file that is no longer the one it read.
//
// A log that did not grow leaves nothing to resume from, and one that shrank was
// truncated or replaced by a smaller file. The equal-length case is a rewrite in
// place, which the mtime is the only witness to; it costs a fold from the
// beginning for a file that has nothing new in it, which is what the caller
// would have done anyway.
func stale(size int64, modTime time.Time, info os.FileInfo) bool {
	return size > info.Size() || (size == info.Size() && !modTime.Equal(info.ModTime()))
}

// stamp records what only the file can say about a fold: when it was last
// written, and which session it belongs to when the log never said.
func stamp(meta Meta, sessionID string, info os.FileInfo) Meta {
	if meta.ID == "" {
		meta.ID = sessionID
	}
	meta.UpdatedAt = info.ModTime().UTC()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = meta.UpdatedAt
	}
	return meta
}

// foldProjection folds whatever a log has grown by since the projection last
// read it, publishing the result on the entry.
//
// The parser the entry holds is the transcript folded so far, which is why a
// fold costs the frames that arrived rather than the file: a session being
// written is followed for what it wrote. The caller holds c.mu.
func (s *Store) foldProjection(c *cached, sessionID, path string, info os.FileInfo) error {
	if stale(c.size, c.modTime, info) {
		c.rewind()
	}
	if c.parser == nil {
		c.parser = newParser(sessionID, s.logger, true)
	}

	at, err := s.readFrom(path, c.progress, c.parser.feed)
	if errors.Is(err, errRewritten) {
		// The log is no longer the file the resume point was taken from, so
		// nothing before it can be trusted: start over and read it whole.
		c.rewind()
		c.parser = newParser(sessionID, s.logger, true)
		at, err = s.readFrom(path, c.progress, c.parser.feed)
	}

	items, meta, headerErr := c.parser.finish()
	if headerErr != nil {
		// A log this build cannot read is not a partial read to wait out: no
		// event in it may be folded on a guess. The parser goes with it, so a
		// log that is later replaced is read on its own terms.
		c.rewind()
		return headerErr
	}
	if err := s.tolerate(err, sessionID, at); err != nil {
		return err
	}

	c.items = slices.Clone(items)
	c.meta = stamp(meta, sessionID, info)
	c.progress = at
	c.size = at.committed
	c.modTime = info.ModTime()
	c.order.Store(s.next())
	return nil
}

// foldMeta folds whatever a log has grown by since a metadata record last read
// it, publishing the result on the record.
//
// The state to resume from is the metadata itself — in this mode a parser
// retains nothing else — so a few thousand records cost a few thousand metadata
// values rather than a few thousand parsers. The caller holds m.mu.
func (s *Store) foldMeta(m *cachedMeta, sessionID, path string, info os.FileInfo) error {
	if stale(m.size, m.modTime, info) {
		m.rewind()
	}

	at, meta, err := s.foldMetaFrom(sessionID, path, m.meta, m.progress)
	if errors.Is(err, errRewritten) {
		m.rewind()
		at, meta, err = s.foldMetaFrom(sessionID, path, m.meta, m.progress)
	}
	if err != nil {
		// foldMetaFrom reports only what a caller cannot serve around, so this
		// is a log that cannot be read rather than one still being written.
		m.rewind()
		return err
	}

	m.publish(stamp(meta, sessionID, info), at, info, s.next())
	return nil
}

// foldMetaFrom folds metadata only, from a resume point.
func (s *Store) foldMetaFrom(sessionID, path string, meta Meta, at progress) (progress, Meta, error) {
	p := metaParser(sessionID, s.logger, meta, at)
	next, err := s.readFrom(path, at, p.feed)
	_, folded, headerErr := p.finish()
	if headerErr != nil {
		return next, Meta{}, headerErr
	}
	if err := s.tolerate(err, sessionID, next); err != nil {
		return next, Meta{}, err
	}
	return next, folded, nil
}

// tolerate decides what a read that stopped early means to its caller.
//
// A tail the writer has not finished is the next read's work rather than this
// one's failure — the frames before it are real history — while a read that
// consumed nothing at all has nothing to serve. Nil is the first answer and the
// error itself the second; a header this build cannot read never reaches here,
// because the parser refuses to fold anything after it.
func (s *Store) tolerate(err error, sessionID string, at progress) error {
	switch {
	case err == nil:
		return nil
	case at.empty():
		return err
	default:
		s.debug("sessionlog: partial read", sessionID, err)
		return nil
	}
}

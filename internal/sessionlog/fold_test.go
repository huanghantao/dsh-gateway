package sessionlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// This file tests the half of the store that reads logs incrementally: that a
// fold costs what was appended rather than what the file holds, that a flush
// caught mid-write is neither lost nor folded twice, and that a log which is no
// longer the one being read is noticed and read again from the start.

// frameOf encodes lines as one zstd frame, the unit DSH appends per flush.
func frameOf(t *testing.T, lines ...string) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer enc.Close()

	var body []byte
	for _, line := range lines {
		body = append(body, line...)
		body = append(body, '\n')
	}
	return enc.EncodeAll(body, nil)
}

// userMessageAt builds a user message event with an explicit sequence.
func userMessageAt(seq int, id, text string) string {
	return fmt.Sprintf(
		`{"type":"user/message","seq":%d,"time":1790746431000,"data":{"content":[{"type":"text","text":%s}],"id":%s,"source":{"kind":"user"}}}`,
		seq, mustJSON(text), mustJSON(id))
}

func mustJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// logPathOf is the file the fixture wrote.
func logPathOf(root string, workspace string) string {
	return filepath.Join(root, workspace, testSession, logFileName)
}

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return info.Size()
}

func writeRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

func appendRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		t.Fatalf("append log: %v", err)
	}
}

// TestAppendCostsWhatWasAppended is the reason this reader exists.
//
// A session being written is read once a second by the live follower, so the
// cost of keeping its projection current has to be the frame that arrived — not
// the whole transcript, which is a decode of everything the session has ever
// said, every second, for as long as it is open.
func TestAppendCostsWhatWasAppended(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)
	ctx := context.Background()

	if _, _, err := s.Items(ctx, testSession); err != nil {
		t.Fatalf("Items: %v", err)
	}
	path := logPathOf(root, "--ws--")
	sizeBefore := sizeOf(t, path)
	before := s.DecodedBytes()

	appendLog(t, root, "--ws--", testSession, userMessageAt(30, "m-user-2", "and now?"))
	appended := uint64(sizeOf(t, path) - sizeBefore)

	if _, _, err := s.Items(ctx, testSession); err != nil {
		t.Fatalf("Items after an append: %v", err)
	}
	if got := s.DecodedBytes() - before; got != appended {
		t.Errorf("an appended frame cost %d decoded bytes, want %d", got, appended)
	}
	if appended >= uint64(sizeBefore) {
		t.Fatalf("the fixture's frame is %d bytes of a %d-byte log: it cannot show the difference",
			appended, sizeBefore)
	}
}

// TestResumedFoldMatchesAFullOne is the equivalence the incremental reader has to
// hold: whatever it does across many reads must land on the projection a reader
// that had never seen the file would build.
func TestResumedFoldMatchesAFullOne(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	ctx := context.Background()

	incremental := newTestStore(t, root)
	items, _, err := incremental.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	seeded := len(items)
	for i := 0; i < 5; i++ {
		appendLog(t, root, "--ws--", testSession, userMessageAt(30+i, fmt.Sprintf("m-%d", i), "another one"))
		items, _, err = incremental.Items(ctx, testSession)
		if err != nil {
			t.Fatalf("Items after append %d: %v", i, err)
		}
		if want := seeded + i + 1; len(items) != want {
			t.Fatalf("after %d appends: %d items, want %d", i+1, len(items), want)
		}
	}

	// A store that has never read the file folds it in one go.
	whole := newTestStore(t, root)
	gotItems, gotMeta, err := incremental.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("incremental Items: %v", err)
	}
	wantItems, wantMeta, err := whole.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("whole Items: %v", err)
	}

	if !reflect.DeepEqual(gotItems, wantItems) {
		t.Errorf("incremental projection has %d items, a full fold has %d; they differ",
			len(gotItems), len(wantItems))
		for i := range gotItems {
			if i >= len(wantItems) || !reflect.DeepEqual(gotItems[i], wantItems[i]) {
				t.Errorf("first difference at item %d:\n incremental: %+v\n whole:       %+v",
					i, gotItems[i], wantItems[i])
				break
			}
		}
	}
	if gotMeta != wantMeta {
		t.Errorf("incremental meta %+v, full fold %+v", gotMeta, wantMeta)
	}
}

// TestAFlushInProgressIsRetriedWithoutRepeatingItself is the hard case for a
// resuming reader: the writer was caught between two halves of a frame, so the
// bytes after the last decodable boundary have been fed once and must not be fed
// again when the rest of the frame arrives.
func TestAFlushInProgressIsRetriedWithoutRepeatingItself(t *testing.T) {
	root := t.TempDir()
	workspace := "--ws--"
	if err := os.MkdirAll(filepath.Join(root, workspace, testSession), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := logPathOf(root, workspace)

	header := fmt.Sprintf(`{"type":"session","version":%d,"id":%q,"cwd":"/tmp/ws"}`, SupportedVersion, testSession)
	first := userMessageAt(1, "m-1", "before the flush")
	second := frameOf(t, userMessageAt(2, "m-2", "inside the flush"))
	writeRaw(t, path, append(frameOf(t, header, first), second[:len(second)/2]...))

	s := newTestStore(t, root)
	ctx := context.Background()

	// The frame before the interrupted one is history and must be served; the
	// half-written frame must not fail the read.
	items, _, err := s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("a flush in progress failed the read: %v", err)
	}
	if len(items) != 1 || items[0].Text != "before the flush" {
		t.Fatalf("items = %+v, want the message before the interrupted frame", items)
	}

	// The flush completes.
	appendRaw(t, path, second[len(second)/2:])

	items, _, err = s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items after the flush completed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("%d items after the flush completed, want 2 (the retry fed a line twice?)", len(items))
	}
	if items[1].Text != "inside the flush" {
		t.Errorf("second item = %+v, want the completed frame's message", items[1])
	}
}

// TestARewrittenLogIsReadFromTheStart covers compaction: the file is replaced by
// one that is not an extension of what was read, so the resume point means
// nothing and the projection has to be rebuilt from the beginning.
func TestARewrittenLogIsReadFromTheStart(t *testing.T) {
	root := t.TempDir()
	workspace := "--ws--"
	writeLog(t, root, workspace, testSession, sampleEvents())
	s := newTestStore(t, root)
	ctx := context.Background()

	if _, _, err := s.Items(ctx, testSession); err != nil {
		t.Fatalf("Items: %v", err)
	}

	// A longer file than the one that was read: the size check cannot see this
	// rewrite, so the frame magic at the resume point is what has to.
	header := fmt.Sprintf(`{"type":"session","version":%d,"id":%q,"cwd":"/tmp/ws"}`, SupportedVersion, testSession)
	lines := []string{header, `{"type":"session/title","seq":2,"time":1790746431000,"data":{"title":"written over"}}`}
	for i := 0; i < 20; i++ {
		lines = append(lines, userMessageAt(10+i, fmt.Sprintf("new-%d", i), "replacement history"))
	}
	writeRaw(t, logPathOf(root, workspace), frameOf(t, lines...))

	items, meta, err := s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items after a rewrite: %v", err)
	}
	if meta.Title != "written over" {
		t.Errorf("title = %q, want the title in the file that replaced the log", meta.Title)
	}
	if len(items) != 20 {
		t.Errorf("%d items, want the 20 in the replacement", len(items))
	}

	// And a shorter file is the other shape of the same thing.
	writeRaw(t, logPathOf(root, workspace), frameOf(t, header, userMessageAt(1, "only", "one message")))
	items, _, err = s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items after a shorter rewrite: %v", err)
	}
	if len(items) != 1 || items[0].Text != "one message" {
		t.Errorf("items = %+v, want only the message in the smaller file", items)
	}
}

// TestMetaResumesToo keeps the session list's cost model honest: the list reads
// every changed log's metadata, so a log being written must cost its append
// there as well.
func TestMetaResumesInsteadOfRereading(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)
	ctx := context.Background()

	if _, err := s.Meta(ctx, testSession); err != nil {
		t.Fatalf("Meta: %v", err)
	}
	before := s.DecodedBytes()
	sizeBefore := sizeOf(t, logPathOf(root, "--ws--"))

	appendLog(t, root, "--ws--", testSession,
		`{"type":"session/title","seq":40,"time":1790746430700,"data":{"title":"a newer title"}}`)
	appended := uint64(sizeOf(t, logPathOf(root, "--ws--")) - sizeBefore)

	meta, err := s.Meta(ctx, testSession)
	if err != nil {
		t.Fatalf("Meta after an append: %v", err)
	}
	if got := s.DecodedBytes() - before; got != appended {
		t.Errorf("a changed log cost %d decoded bytes, want the %d that were appended", got, appended)
	}
	if meta.Title != "a newer title" {
		t.Errorf("title = %q, want the appended one", meta.Title)
	}
	if meta.Preview == "" || meta.Messages == 0 {
		t.Errorf("resumed metadata lost what the earlier frames carried: %+v", meta)
	}
}

// TestDecodeAllKeepsTheFramesBeforeATruncatedOne pins the third-party behaviour
// the resume logic is built on: a truncated frame is an error, and the frames
// before it are returned anyway. If a future version of the decoder returned
// nothing, a reader catching a flush mid-write would go blind until the writer
// finished; if it returned success, the resume point would move past bytes that
// were never folded.
func TestDecodeAllKeepsTheFramesBeforeATruncatedOne(t *testing.T) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd reader: %v", err)
	}
	defer dec.Close()

	first, second := frameOf(t, "first"), frameOf(t, "second")
	whole, err := dec.DecodeAll(append(append([]byte{}, first...), second...), nil)
	if err != nil {
		t.Fatalf("decoding two whole frames: %v", err)
	}
	if string(whole) != "first\nsecond\n" {
		t.Fatalf("two frames decoded to %q", whole)
	}

	truncated := append(append([]byte{}, first...), second[:len(second)/2]...)
	partial, err := dec.DecodeAll(truncated, nil)
	if err == nil {
		t.Fatal("a truncated frame decoded without an error, so a resume point could skip it")
	}
	if string(partial) != "first\n" {
		t.Errorf("decoded %q before the truncated frame, want the whole frames that precede it", partial)
	}
}

// TestConcurrentReadsAreConsistent is what the entry lock is for: a fold resumes
// in place, so two readers of one session must not fold it at the same time —
// and a reader must never see a projection assembled by two of them at once.
func TestConcurrentReadsAreConsistent(t *testing.T) {
	root := t.TempDir()
	writeLog(t, root, "--ws--", testSession, sampleEvents())
	s := newTestStore(t, root)
	ctx := context.Background()
	path := logPathOf(root, "--ws--")

	items, _, err := s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	seeded := len(items)

	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, _, err := s.Items(ctx, testSession); err != nil {
					t.Errorf("Items: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 10; i++ {
		appendRaw(t, path, frameOf(t, userMessageAt(50+i, fmt.Sprintf("m-%d", i), "appended while reading")))
	}
	wg.Wait()

	items, _, err = s.Items(ctx, testSession)
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	rows := 0
	for _, item := range items {
		if item.Text == "appended while reading" {
			rows++
		}
	}
	if rows != 10 {
		t.Fatalf("the appends folded to %d of 10 rows", rows)
	}
	if len(items) != seeded+10 {
		t.Errorf("%d items after 10 appends, want %d", len(items), seeded+10)
	}
}

package live

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scriberr/internal/models"
)

func TestChunkStoreAppendDuplicateGap(t *testing.T) {
	dir := t.TempDir()
	s, err := openChunkStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.close()

	chunks := [][]byte{[]byte("aaaa"), []byte("bb"), []byte("cccccc")}
	for i, c := range chunks {
		if err := s.append(i, c); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if s.nextSeq != 3 || s.size != 12 {
		t.Fatalf("nextSeq=%d size=%d, want 3 and 12", s.nextSeq, s.size)
	}

	// A retried chunk is a duplicate and is not appended again.
	if err := s.append(1, []byte("bb")); !errors.Is(err, errDuplicateChunk) {
		t.Fatalf("duplicate: got %v", err)
	}
	// A chunk from the future is a gap.
	if err := s.append(5, []byte("zz")); !errors.Is(err, errChunkGap) {
		t.Fatalf("gap: got %v", err)
	}
	if s.nextSeq != 3 || s.size != 12 {
		t.Fatalf("after dup/gap nextSeq=%d size=%d, want 3 and 12", s.nextSeq, s.size)
	}
	if err := s.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	rec, _ := os.ReadFile(filepath.Join(dir, recordingFileName))
	if string(rec) != "aaaabbcccccc" {
		t.Fatalf("recording = %q", rec)
	}
	idx, _ := os.ReadFile(filepath.Join(dir, indexFileName))
	if want := "0 0 4\n1 4 2\n2 6 6\n"; string(idx) != want {
		t.Fatalf("index = %q, want %q", idx, want)
	}
}

func TestChunkStoreReopenTruncates(t *testing.T) {
	dir := t.TempDir()
	s, err := openChunkStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := s.append(i, bytes.Repeat([]byte{byte('a' + i)}, 10)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	s.close()

	// Simulate a crash mid-append: unindexed bytes in the recording and a torn
	// index line.
	appendFile(t, filepath.Join(dir, recordingFileName), []byte("GARBAGE-NOT-ACKED"))
	appendFile(t, filepath.Join(dir, indexFileName), []byte("4 40 1"))

	s, err = openChunkStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if s.nextSeq != 4 || s.size != 40 {
		t.Fatalf("reopen nextSeq=%d size=%d, want 4 and 40", s.nextSeq, s.size)
	}
	if info, _ := os.Stat(filepath.Join(dir, recordingFileName)); info.Size() != 40 {
		t.Fatalf("recording not truncated: %d bytes", info.Size())
	}
	if err := s.append(4, []byte("eeeee")); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	s.close()

	idx, _ := os.ReadFile(filepath.Join(dir, indexFileName))
	if !strings.HasSuffix(string(idx), "3 30 10\n4 40 5\n") {
		t.Fatalf("index after reopen = %q", idx)
	}
	rec, _ := os.ReadFile(filepath.Join(dir, recordingFileName))
	if len(rec) != 45 || string(rec[40:]) != "eeeee" {
		t.Fatalf("recording after reopen = %q", rec)
	}
}

func TestChunkStoreReopenDropsEntriesMissingData(t *testing.T) {
	dir := t.TempDir()
	s, _ := openChunkStore(dir)
	for i := 0; i < 3; i++ {
		_ = s.append(i, []byte("0123456789"))
	}
	s.close()

	// The index survived but the last chunk's data did not (lost unsynced write).
	if err := os.Truncate(filepath.Join(dir, recordingFileName), 25); err != nil {
		t.Fatal(err)
	}
	s, err := openChunkStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.close()
	if s.nextSeq != 2 || s.size != 20 {
		t.Fatalf("nextSeq=%d size=%d, want 2 and 20", s.nextSeq, s.size)
	}
	if next, size, _ := indexSummary(dir); next != 2 || size != 20 {
		t.Fatalf("indexSummary = %d, %d", next, size)
	}
}

func appendFile(t *testing.T, path string, data []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

// TestTailReaderBlocksThenEOF also checks that the recording can be opened for
// reading while the chunk store holds it open for appending (Windows sharing).
func TestTailReaderBlocksThenEOF(t *testing.T) {
	dir := t.TempDir()
	store, err := openChunkStore(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.close()

	g := newGrowth(0)
	tail, err := openTail(store.recordingPath(), g)
	if err != nil {
		t.Fatalf("open tail while the writer has the file open: %v", err)
	}
	defer tail.Close()

	type result struct {
		data []byte
		err  error
	}
	results := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(tail)
		results <- result{data, err}
	}()

	// Nothing committed yet: the reader must block.
	select {
	case r := <-results:
		t.Fatalf("reader returned early: %q %v", r.data, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	for i, c := range []string{"hello ", "live ", "world"} {
		if err := store.append(i, []byte(c)); err != nil {
			t.Fatal(err)
		}
		g.set(store.size)
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case r := <-results:
		t.Fatalf("reader returned before the input ended: %q %v", r.data, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	g.close()
	select {
	case r := <-results:
		if r.err != nil {
			t.Fatalf("read error: %v", r.err)
		}
		if string(r.data) != "hello live world" {
			t.Fatalf("read %q", r.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not return io.EOF after close")
	}
}

func TestTailReaderCloseUnblocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	tail, err := openTail(path, newGrowth(0))
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := tail.Read(make([]byte, 16))
		errs <- err
	}()
	time.Sleep(100 * time.Millisecond)
	tail.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, errTailClosed) {
			t.Fatalf("got %v, want errTailClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestMaterializeWordTimingFallback(t *testing.T) {
	words := `[{"word":"Hello","start":10.0,"end":10.4},{"word":"there","start":10.5,"end":10.9}]`
	segs := []models.LiveSegment{
		{Seq: 0, StartSec: 10, EndSec: 11, Text: "Hello there", WordsJSON: &words},
		{Seq: 1, StartSec: 12, EndSec: 14, Text: "  one two three four  "}, // no worker words
		{Seq: 2, StartSec: 15, EndSec: 16, Text: "   "},                    // empty text is skipped
		{Seq: 3, StartSec: 17, EndSec: 17, Text: "instant"},                // zero-length span
	}
	res := materialize(segs, "nemotron-en-1120ms")

	if res.Text != "Hello there one two three four instant" {
		t.Fatalf("text = %q", res.Text)
	}
	if res.Language != "en" || res.ModelUsed != "live:nemotron-en-1120ms" {
		t.Fatalf("language=%q model_used=%q", res.Language, res.ModelUsed)
	}
	if len(res.Segments) != 3 {
		t.Fatalf("segments = %d, want 3", len(res.Segments))
	}
	if len(res.WordSegments) != 7 {
		t.Fatalf("word_segments = %d, want 7: %+v", len(res.WordSegments), res.WordSegments)
	}
	// Worker timings are kept as is.
	if w := res.WordSegments[1]; w.Word != "there" || w.Start != 10.5 || w.End != 10.9 {
		t.Fatalf("worker word = %+v", w)
	}
	// The fallback spreads 12..14 evenly over four words.
	wantStarts := []float64{12, 12.5, 13, 13.5}
	for i, ws := range wantStarts {
		w := res.WordSegments[2+i]
		if w.Start != ws || w.End != ws+0.5 {
			t.Fatalf("fallback word %d = %+v, want start %.1f", i, w, ws)
		}
	}
	if w := res.WordSegments[6]; w.Word != "instant" || w.Start != 17 || w.End != 17 {
		t.Fatalf("zero-span word = %+v", w)
	}

	// The JSON shape the detail view reads.
	data, _ := json.Marshal(res)
	var shape map[string]interface{}
	_ = json.Unmarshal(data, &shape)
	for _, key := range []string{"text", "language", "segments", "word_segments", "model_used"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("transcript JSON lacks %q: %s", key, data)
		}
	}

	// No segments: empty but valid JSON arrays.
	empty := materialize(nil, "x")
	data, _ = json.Marshal(empty)
	if !strings.Contains(string(data), `"segments":[]`) {
		t.Fatalf("empty transcript = %s", data)
	}
}

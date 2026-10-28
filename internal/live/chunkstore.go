package live

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Files in a live session directory (data/uploads/live/<job_id>/)
const (
	recordingFileName = "recording.webm"       // append-only raw MediaRecorder stream (the fallback)
	indexFileName     = "chunks.idx"           // "<seq> <byte_offset> <byte_len>\n" per accepted chunk
	finalFileName     = "recording.final.webm" // seekable remux written at finalize
	workerLogFileName = "worker.log"           // stderr of ffmpeg and the ASR worker
	metaFileName      = "session.json"         // session settings that must survive restarts
)

// syncEvery is how many appended chunks may stay un-fsynced
const syncEvery = 5

var (
	errDuplicateChunk = errors.New("duplicate chunk")
	errChunkGap       = errors.New("chunk gap")
)

// indexEntry is one parsed chunks.idx line
type indexEntry struct {
	seq     int
	offset  int64
	length  int64
	lineEnd int64 // byte offset in chunks.idx just past this line
}

// chunkStore owns recording.webm and chunks.idx of one session. It is not safe
// for concurrent use; the session mutex serializes access.
type chunkStore struct {
	dir      string
	rec      *os.File
	idx      *os.File
	nextSeq  int
	size     int64 // end offset of the last indexed chunk
	idxSize  int64
	unsynced int
}

// readIndex parses chunks.idx and returns its valid prefix: sequential seqs that
// start at 0, with contiguous offsets. A torn trailing line (crash mid-write) or
// anything after the first inconsistency is ignored.
func readIndex(path string) ([]indexEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var entries []indexEntry
	var pos, end int64
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		if nl < 0 {
			break // unterminated line
		}
		fields := bytes.Fields(data[:nl])
		if len(fields) != 3 {
			break
		}
		seq, err1 := strconv.Atoi(string(fields[0]))
		off, err2 := strconv.ParseInt(string(fields[1]), 10, 64)
		n, err3 := strconv.ParseInt(string(fields[2]), 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || seq != len(entries) || off != end || n < 0 {
			break
		}
		pos += int64(nl) + 1
		end += n
		entries = append(entries, indexEntry{seq: seq, offset: off, length: n, lineEnd: pos})
		data = data[nl+1:]
	}
	return entries, nil
}

// indexSummary returns next_seq and the recording end offset from chunks.idx
// without opening the session for writing.
func indexSummary(dir string) (nextSeq int, size int64, err error) {
	entries, err := readIndex(filepath.Join(dir, indexFileName))
	if err != nil || len(entries) == 0 {
		return 0, 0, err
	}
	last := entries[len(entries)-1]
	return len(entries), last.offset + last.length, nil
}

// openChunkStore opens (or creates) a session's recording and index. On reopen it
// truncates chunks.idx to its valid prefix and recording.webm to the last indexed
// end offset, so bytes that were never acknowledged are dropped.
func openChunkStore(dir string) (*chunkStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create session directory: %w", err)
	}

	idxPath := filepath.Join(dir, indexFileName)
	entries, err := readIndex(idxPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk index: %w", err)
	}

	rec, err := os.OpenFile(filepath.Join(dir, recordingFileName), os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open recording: %w", err)
	}
	recInfo, err := rec.Stat()
	if err != nil {
		rec.Close()
		return nil, err
	}

	// The index may claim bytes the recording lost (unsynced data at power loss):
	// keep only entries that are fully present.
	for len(entries) > 0 {
		last := entries[len(entries)-1]
		if last.offset+last.length <= recInfo.Size() {
			break
		}
		entries = entries[:len(entries)-1]
	}

	s := &chunkStore{dir: dir, rec: rec}
	if len(entries) > 0 {
		last := entries[len(entries)-1]
		s.nextSeq = len(entries)
		s.size = last.offset + last.length
		s.idxSize = last.lineEnd
	}

	if recInfo.Size() != s.size {
		if err := rec.Truncate(s.size); err != nil {
			rec.Close()
			return nil, fmt.Errorf("failed to truncate recording: %w", err)
		}
	}
	if _, err := rec.Seek(s.size, 0); err != nil {
		rec.Close()
		return nil, err
	}

	idx, err := os.OpenFile(idxPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		rec.Close()
		return nil, fmt.Errorf("failed to open chunk index: %w", err)
	}
	if info, err := idx.Stat(); err == nil && info.Size() != s.idxSize {
		if err := idx.Truncate(s.idxSize); err != nil {
			rec.Close()
			idx.Close()
			return nil, fmt.Errorf("failed to truncate chunk index: %w", err)
		}
	}
	if _, err := idx.Seek(s.idxSize, 0); err != nil {
		rec.Close()
		idx.Close()
		return nil, err
	}
	s.idx = idx
	return s, nil
}

// append writes chunk seq at the end of the recording and indexes it. It returns
// errDuplicateChunk for seq < nextSeq and errChunkGap for seq > nextSeq.
func (s *chunkStore) append(seq int, data []byte) error {
	if s.rec == nil {
		return os.ErrClosed
	}
	if seq < s.nextSeq {
		return errDuplicateChunk
	}
	if seq > s.nextSeq {
		return errChunkGap
	}

	if _, err := s.rec.Write(data); err != nil {
		s.rollback()
		return fmt.Errorf("failed to append chunk: %w", err)
	}
	line := fmt.Sprintf("%d %d %d\n", seq, s.size, len(data))
	if _, err := s.idx.Write([]byte(line)); err != nil {
		s.rollback()
		return fmt.Errorf("failed to index chunk: %w", err)
	}

	s.size += int64(len(data))
	s.idxSize += int64(len(line))
	s.nextSeq++
	s.unsynced++
	if s.unsynced >= syncEvery {
		return s.sync()
	}
	return nil
}

// rollback drops a partially written chunk so the files match the index again
func (s *chunkStore) rollback() {
	_ = s.rec.Truncate(s.size)
	_, _ = s.rec.Seek(s.size, 0)
	_ = s.idx.Truncate(s.idxSize)
	_, _ = s.idx.Seek(s.idxSize, 0)
}

// sync flushes both files to disk
func (s *chunkStore) sync() error {
	if s.rec == nil {
		return nil
	}
	if err := s.rec.Sync(); err != nil {
		return fmt.Errorf("failed to sync recording: %w", err)
	}
	if err := s.idx.Sync(); err != nil {
		return fmt.Errorf("failed to sync chunk index: %w", err)
	}
	s.unsynced = 0
	return nil
}

// close syncs and closes the files. It is safe to call more than once.
func (s *chunkStore) close() error {
	if s.rec == nil {
		return nil
	}
	err := s.sync()
	if cerr := s.rec.Close(); err == nil {
		err = cerr
	}
	if cerr := s.idx.Close(); err == nil {
		err = cerr
	}
	s.rec, s.idx = nil, nil
	return err
}

func (s *chunkStore) recordingPath() string {
	return filepath.Join(s.dir, recordingFileName)
}

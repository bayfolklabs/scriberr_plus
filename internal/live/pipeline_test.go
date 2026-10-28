package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"scriberr/internal/models"
	"scriberr/internal/transcription/interfaces"
)

// feed uploads chunks starting at seq first, failing the test on any error
func feed(t *testing.T, m *Manager, id string, first int, chunks [][]byte) int {
	t.Helper()
	seq := first
	for _, c := range chunks {
		ack, err := m.AppendChunk(context.Background(), id, seq, c)
		if err != nil {
			t.Fatalf("append seq %d: %v", seq, err)
		}
		if ack.Ack != seq || ack.NextSeq != seq+1 || ack.Duplicate {
			t.Fatalf("append seq %d: ack %+v", seq, ack)
		}
		seq++
	}
	return seq
}

func decodeTranscript(t *testing.T, s *string) interfaces.TranscriptResult {
	t.Helper()
	if s == nil {
		t.Fatal("transcript is nil")
	}
	var res interfaces.TranscriptResult
	if err := json.Unmarshal([]byte(*s), &res); err != nil {
		t.Fatalf("transcript JSON: %v", err)
	}
	return res
}

// checkSegments verifies seqs are contiguous and times never go backwards
func checkSegments(t *testing.T, segs []models.LiveSegment) {
	t.Helper()
	for i, seg := range segs {
		if seg.Seq != i {
			t.Fatalf("segment %d has seq %d", i, seg.Seq)
		}
		if i > 0 && seg.StartSec < segs[i-1].EndSec-1e-6 {
			t.Fatalf("segment %d starts at %.3f before previous end %.3f (duplicate text?)", i, seg.StartSec, segs[i-1].EndSec)
		}
	}
}

func TestPipelineWithFakeWorker(t *testing.T) {
	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), true)
	ctx := context.Background()

	job, err := m.CreateSession(ctx, CreateRequest{Title: "Weekly sync", MimeType: "audio/webm;codecs=opus"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if job.Source != models.SourceLive || job.LiveStatus != models.LiveStatusRecording || job.Status != models.StatusUploaded {
		t.Fatalf("job = %+v", job)
	}

	audio := pcm(5.5)
	chunks := split(audio, sampleRate) // 0.5 s per chunk
	next := feed(t, m, job.ID, 0, chunks)

	// Duplicate and gap handling at the manager level.
	if ack, err := m.AppendChunk(ctx, job.ID, 3, chunks[3]); err != nil || !ack.Duplicate || ack.NextSeq != next {
		t.Fatalf("duplicate: %+v %v", ack, err)
	}
	var liveErr *Error
	if _, err := m.AppendChunk(ctx, job.ID, next+2, []byte("x")); !errors.As(err, &liveErr) || liveErr.Code != CodeGap || liveErr.NextSeq != next {
		t.Fatalf("gap: %v", err)
	}

	waitFor(t, 20*time.Second, "live segments", func() bool {
		st, err := m.State(ctx, job.ID, -1)
		return err == nil && len(st.Segments) >= 2 && st.ASR.State == asrRunning
	})
	st, _ := m.State(ctx, job.ID, -1)
	if st.Title != "Weekly sync" || st.NextSeq != next || st.Bytes != int64(len(audio)) || st.ASR.Engine != "fake" {
		t.Fatalf("state = %+v", st)
	}
	if after, _ := m.State(ctx, job.ID, 0); len(after.Segments) != len(st.Segments)-1 || after.Segments[0].Seq != 1 {
		t.Fatalf("after=0 returned %+v", after.Segments)
	}

	// Stop with a chunk the server never received.
	if _, err := m.Stop(ctx, job.ID, next); !errors.As(err, &liveErr) || liveErr.Code != CodeMissingChunks || liveErr.NextSeq != next {
		t.Fatalf("stop missing chunks: %v", err)
	}
	s := m.get(job.ID)
	status, err := m.Stop(ctx, job.ID, next-1)
	if err != nil || status != models.LiveStatusFinalizing {
		t.Fatalf("stop: %s %v", status, err)
	}
	if _, err := m.AppendChunk(ctx, job.ID, next, []byte("late")); !errors.As(err, &liveErr) || liveErr.Code != CodeClosed {
		t.Fatalf("chunk after stop: %v", err)
	}

	done := waitFinalized(t, db, job.ID)
	s.asrMu.Lock()
	got, asrState, restarts := s.asr.receivedSec, s.asr.state, s.asr.restarts
	s.asrMu.Unlock()
	if asrState != asrStopped || restarts != 0 {
		t.Fatalf("asr state=%s restarts=%d after a clean stop", asrState, restarts)
	}
	if math.Abs(got-5.5) > 0.001 {
		t.Fatalf("received_audio_sec = %.3f, want 5.5", got)
	}
	if done.Status != models.StatusCompleted || done.TranscriptSource != models.TranscriptSourceLive {
		t.Fatalf("status=%s transcript_source=%s", done.Status, done.TranscriptSource)
	}
	res := decodeTranscript(t, done.Transcript)
	live := decodeTranscript(t, done.LiveTranscript)
	if res.Text != live.Text {
		t.Fatal("transcript and live_transcript differ")
	}
	// 0-2, 2-4 and the flushed 4-5.5 tail
	if len(res.Segments) != 3 || res.Segments[2].End != 5.5 {
		t.Fatalf("segments = %+v", res.Segments)
	}
	if len(res.WordSegments) != 12 {
		t.Fatalf("word_segments = %d, want 12", len(res.WordSegments))
	}

	var segs []models.LiveSegment
	db.Where("transcription_job_id = ?", job.ID).Order("seq").Find(&segs)
	if len(segs) != 3 {
		t.Fatalf("persisted segments = %d", len(segs))
	}
	checkSegments(t, segs)

	// The raw recording is never modified; PCM is not WebM, so the remux fell back.
	rec, _ := os.ReadFile(filepath.Join(filepath.Dir(done.AudioPath), recordingFileName))
	if !bytes.Equal(rec, audio) {
		t.Fatal("recording.webm differs from the uploaded chunks")
	}

	// Stopping again is idempotent; state is served from disk now.
	if status, err := m.Stop(ctx, job.ID, next-1); err != nil || status != models.LiveStatusFinalized {
		t.Fatalf("second stop: %s %v", status, err)
	}
	st, err = m.State(ctx, job.ID, -1)
	if err != nil || st.LiveStatus != models.LiveStatusFinalized || len(st.Segments) != 3 || st.NextSeq != next ||
		st.ASR.State != asrStopped || st.ReceivedAudioSec != 5.5 {
		t.Fatalf("state after finalize = %+v %v", st, err)
	}
}

func TestPipelineRestartsCrashedWorker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "crashed")
	t.Setenv(crashMarkerEnv, marker)

	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), true)
	ctx := context.Background()

	job, err := m.CreateSession(ctx, CreateRequest{Title: "crash"})
	if err != nil {
		t.Fatal(err)
	}
	next := feed(t, m, job.ID, 0, split(pcm(9), sampleRate))

	waitFor(t, 30*time.Second, "worker restart and catch-up", func() bool {
		st, _ := m.State(ctx, job.ID, -1)
		return st.ASR.Restarts >= 1 && st.ASR.State == asrRunning && len(st.Segments) >= 4
	})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("fake worker never crashed")
	}
	if _, err := m.Stop(ctx, job.ID, next-1); err != nil {
		t.Fatal(err)
	}
	done := waitFinalized(t, db, job.ID)

	var segs []models.LiveSegment
	db.Where("transcription_job_id = ?", job.ID).Order("seq").Find(&segs)
	checkSegments(t, segs)
	// Run 1 produced 0-2 then crashed at 3 s; run 2 resumed at 2 s: 2-4, 4-6, 6-8, 8-9.
	if len(segs) != 5 || segs[1].StartSec != 2 || segs[4].EndSec != 9 {
		t.Fatalf("segments after restart = %+v", segs)
	}
	if res := decodeTranscript(t, done.Transcript); len(res.Segments) != 5 {
		t.Fatalf("transcript segments = %d", len(res.Segments))
	}
}

func TestReopenAfterRestart(t *testing.T) {
	db := newTestDB(t)
	uploads := t.TempDir()
	ctx := context.Background()

	m1 := newTestManager(t, db, uploads, true)
	job, err := m1.CreateSession(ctx, CreateRequest{Title: "restart", AutoRetranscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	first := split(pcm(5), sampleRate)
	next := feed(t, m1, job.ID, 0, first)
	waitFor(t, 20*time.Second, "first segments", func() bool {
		st, _ := m1.State(ctx, job.ID, -1)
		return len(st.Segments) >= 2
	})
	shutdownManager(t, m1) // server shutdown: the session stays 'recording'
	if got := loadJob(t, db, job.ID).LiveStatus; got != models.LiveStatusRecording {
		t.Fatalf("live_status after shutdown = %s", got)
	}

	// A crash mid-append left unacknowledged bytes and a torn index line.
	dir := filepath.Join(uploads, "live", job.ID)
	appendFile(t, filepath.Join(dir, recordingFileName), []byte("torn"))
	appendFile(t, filepath.Join(dir, indexFileName), []byte(strconv.Itoa(next)+" 1"))

	m2 := newTestManager(t, db, uploads, true)
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := m2.State(ctx, job.ID, -1)
	if err != nil || st.LiveStatus != models.LiveStatusInterrupted || st.NextSeq != next || !st.AutoRetranscribe || st.ReceivedAudioSec != 5 {
		t.Fatalf("state after restart = %+v %v", st, err)
	}
	list, err := m2.List(ctx, nil)
	if err != nil || len(list) != 1 || list[0].ID != job.ID || list[0].NextSeq != next {
		t.Fatalf("list = %+v %v", list, err)
	}
	segsBefore := len(st.Segments)

	// The client resumes: a gap reopens the session but is rejected with next_seq.
	var liveErr *Error
	if _, err := m2.AppendChunk(ctx, job.ID, next+1, []byte("x")); !errors.As(err, &liveErr) || liveErr.Code != CodeGap || liveErr.NextSeq != next {
		t.Fatalf("gap after reopen: %v", err)
	}
	if got := loadJob(t, db, job.ID).LiveStatus; got != models.LiveStatusRecording {
		t.Fatalf("live_status after reopen = %s", got)
	}
	second := split(pcm(3), sampleRate)
	next = feed(t, m2, job.ID, next, second)
	if info, _ := os.Stat(filepath.Join(dir, recordingFileName)); info.Size() != int64(len(pcm(8))) {
		t.Fatalf("recording size = %d, want %d (torn bytes not truncated?)", info.Size(), len(pcm(8)))
	}

	queued := make(chan string, 1)
	m2.SetRetranscriber(func(ctx context.Context, jobID string, owner *uint) error {
		queued <- jobID
		return nil
	})
	if _, err := m2.Stop(ctx, job.ID, next-1); err != nil {
		t.Fatal(err)
	}
	waitFinalized(t, db, job.ID)

	var segs []models.LiveSegment
	db.Where("transcription_job_id = ?", job.ID).Order("seq").Find(&segs)
	checkSegments(t, segs)
	if len(segs) <= segsBefore || segs[len(segs)-1].EndSec != 8 {
		t.Fatalf("segments after resume = %+v", segs)
	}
	select {
	case id := <-queued:
		if id != job.ID {
			t.Fatalf("re-transcription queued for %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("auto re-transcription was not queued")
	}
}

func TestFinalizeResumesAfterRestart(t *testing.T) {
	db := newTestDB(t)
	uploads := t.TempDir()
	ctx := context.Background()

	m1 := newTestManager(t, db, uploads, true)
	job, err := m1.CreateSession(ctx, CreateRequest{Title: "finalize cut off"})
	if err != nil {
		t.Fatal(err)
	}
	feed(t, m1, job.ID, 0, split(pcm(3), sampleRate))
	shutdownManager(t, m1)
	// The server stopped while finalizing.
	if err := db.Model(&models.TranscriptionJob{}).Where("id = ?", job.ID).Update("live_status", models.LiveStatusFinalizing).Error; err != nil {
		t.Fatal(err)
	}

	exe := testExe(t)
	m2 := NewManager(db, Options{UploadDir: uploads, EnvDir: filepath.Join(uploads, "env"), WorkerCmd: exe + " fake-live-worker"})
	m2.decoderCmd = []string{exe, "fake-decoder"}
	t.Cleanup(func() { shutdownManager(t, m2) })
	if err := m2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	m2.Start()
	done := waitFinalized(t, db, job.ID)
	if res := decodeTranscript(t, done.Transcript); len(res.Segments) != 2 || res.Segments[1].End != 3 {
		t.Fatalf("segments = %+v", res.Segments)
	}
}

func TestJanitorSuspendsAndFinalizesAbandoned(t *testing.T) {
	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), true)
	ctx := context.Background()

	job, err := m.CreateSession(ctx, CreateRequest{Title: "abandoned"})
	if err != nil {
		t.Fatal(err)
	}
	next := feed(t, m, job.ID, 0, split(pcm(2.5), sampleRate))
	waitFor(t, 20*time.Second, "segment", func() bool {
		st, _ := m.State(ctx, job.ID, -1)
		return len(st.Segments) >= 1
	})

	// The browser vanished: no chunks for longer than AbandonAfter.
	m.opts.AbandonAfter = 50 * time.Millisecond
	time.Sleep(100 * time.Millisecond)
	m.sweep() // recording -> interrupted (pipeline stopped, still resumable)
	if m.get(job.ID) != nil {
		t.Fatal("idle session still open after sweep")
	}
	if got := loadJob(t, db, job.ID).LiveStatus; got != models.LiveStatusInterrupted {
		t.Fatalf("live_status = %s, want interrupted", got)
	}

	time.Sleep(100 * time.Millisecond)
	m.sweep() // interrupted and idle -> finalized
	done := waitFinalized(t, db, job.ID)
	res := decodeTranscript(t, done.Transcript)
	// Finalize drained the rest of the recording (2-2.5 s) through the worker.
	if len(res.Segments) != 2 || res.Segments[1].End != 2.5 {
		t.Fatalf("segments = %+v", res.Segments)
	}
	var liveErr *Error
	if _, err := m.AppendChunk(ctx, job.ID, next, []byte("x")); !errors.As(err, &liveErr) || liveErr.Code != CodeClosed || liveErr.LiveStatus != models.LiveStatusFinalized {
		t.Fatalf("chunk after finalize: %v", err)
	}
}

// TestASRStartsWhenEnvironmentBecomesReady records while the live environment is
// still being prepared (decode only, asr.state=starting), then starts the worker
// mid-session once it is ready, and transcribes from the beginning.
func TestASRStartsWhenEnvironmentBecomesReady(t *testing.T) {
	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), true)
	ctx := context.Background()
	m.env = &pythonEnv{state: envPreparing, ready: make(chan struct{})}

	job, err := m.CreateSession(ctx, CreateRequest{Title: "env"})
	if err != nil {
		t.Fatal(err)
	}
	next := feed(t, m, job.ID, 0, split(pcm(3), sampleRate))
	waitFor(t, 10*time.Second, "decode-only progress", func() bool {
		st, _ := m.State(ctx, job.ID, -1)
		return st.ReceivedAudioSec == 3
	})
	if st, _ := m.State(ctx, job.ID, -1); st.ASR.State != asrStarting || len(st.Segments) != 0 {
		t.Fatalf("state while preparing = %+v", st.ASR)
	}

	// Preparation failed: unavailable, but recording continues.
	m.env.mu.Lock()
	m.env.state, m.env.errMsg = envFailed, "uv sync failed"
	m.env.mu.Unlock()
	next = feed(t, m, job.ID, next, split(pcm(2), sampleRate))
	if st, _ := m.State(ctx, job.ID, -1); st.ASR.State != asrUnavailable || st.ASR.Error != "uv sync failed" {
		t.Fatalf("state after env failure = %+v", st.ASR)
	}

	// A retry succeeded: the worker starts and catches up from 0 s.
	m.env.mu.Lock()
	m.env.state, m.env.errMsg = envReady, ""
	close(m.env.ready)
	m.env.mu.Unlock()
	waitFor(t, 20*time.Second, "worker after env ready", func() bool {
		st, _ := m.State(ctx, job.ID, -1)
		return st.ASR.State == asrRunning && len(st.Segments) >= 2
	})
	if st, _ := m.State(ctx, job.ID, -1); st.Segments[0].Start != 0 || st.ASR.Restarts != 0 {
		t.Fatalf("state after upgrade = %+v", st)
	}
	if _, err := m.Stop(ctx, job.ID, next-1); err != nil {
		t.Fatal(err)
	}
	done := waitFinalized(t, db, job.ID)
	if res := decodeTranscript(t, done.Transcript); len(res.Segments) != 3 || res.Segments[2].End != 5 {
		t.Fatalf("segments = %+v", res.Segments)
	}
}

func TestEmptySessionFinalizes(t *testing.T) {
	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), true)
	job, err := m.CreateSession(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(*job.Title, "Live session ") {
		t.Fatalf("default title = %q", *job.Title)
	}
	if _, err := m.Finalize(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	done := waitFinalized(t, db, job.ID)
	if res := decodeTranscript(t, done.Transcript); res.Text != "" || len(res.Segments) != 0 {
		t.Fatalf("empty session transcript = %+v", res)
	}
}

// TestPipelineFFmpegWebM feeds a real WebM/Opus stream (as MediaRecorder would
// produce it) through the real ffmpeg decoder and the fake worker.
func TestPipelineFFmpegWebM(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	wav, err := filepath.Abs(filepath.Join("..", "..", "tests", "data", "AMI-Corpus-IB4002.Mix-Headset-clip.wav"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wav); err != nil {
		t.Skip("test clip not found")
	}

	// Encode to a streaming (non-seekable) WebM on stdout, like MediaRecorder.
	var webm, stderr bytes.Buffer
	enc := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-i", wav,
		"-c:a", "libopus", "-b:a", "64k", "-f", "webm", "pipe:1")
	enc.Stdout, enc.Stderr = &webm, &stderr
	if err := enc.Run(); err != nil {
		t.Fatalf("encode webm: %v: %s", err, stderr.String())
	}

	db := newTestDB(t)
	m := newTestManager(t, db, t.TempDir(), false)
	ctx := context.Background()
	job, err := m.CreateSession(ctx, CreateRequest{Title: "webm", MimeType: "audio/webm;codecs=opus"})
	if err != nil {
		t.Fatal(err)
	}
	s := m.get(job.ID)

	chunks := split(webm.Bytes(), 16*1024)
	next := 0
	for _, c := range chunks {
		next = feed(t, m, job.ID, next, [][]byte{c})
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, 30*time.Second, "segments from webm", func() bool {
		st, _ := m.State(ctx, job.ID, -1)
		return len(st.Segments) >= 3
	})

	if _, err := m.Stop(ctx, job.ID, next-1); err != nil {
		t.Fatal(err)
	}
	done := waitFinalized(t, db, job.ID)

	s.asrMu.Lock()
	received, asrState, restarts := s.asr.receivedSec, s.asr.state, s.asr.restarts
	s.asrMu.Unlock()
	if asrState != asrStopped || restarts != 0 {
		t.Fatalf("asr state=%s restarts=%d after a clean stop", asrState, restarts)
	}
	if math.Abs(received-19.4) > 0.3 {
		t.Fatalf("received_audio_sec = %.3f, want about 19.4", received)
	}
	res := decodeTranscript(t, done.Transcript)
	if len(res.Segments) != 10 || len(res.WordSegments) == 0 {
		t.Fatalf("segments = %d, words = %d", len(res.Segments), len(res.WordSegments))
	}
	if filepath.Base(done.AudioPath) != finalFileName {
		t.Fatalf("audio_path = %s, want the remuxed %s", done.AudioPath, finalFileName)
	}
	if info, err := os.Stat(done.AudioPath); err != nil || info.Size() == 0 {
		t.Fatalf("remuxed file: %v", err)
	}
	if out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", done.AudioPath).Output(); err == nil {
		if d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err != nil || math.Abs(d-19.4) > 0.3 {
			t.Fatalf("remuxed duration = %q", out)
		}
	}
}

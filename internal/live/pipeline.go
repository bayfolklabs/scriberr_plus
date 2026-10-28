package live

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"scriberr/internal/models"
	"scriberr/pkg/logger"
)

// ASR states reported in GET /state
const (
	asrStarting    = "starting"
	asrRunning     = "running"
	asrRestarting  = "restarting"
	asrStopped     = "stopped"
	asrFailed      = "failed"
	asrUnavailable = "unavailable"
)

const (
	sampleRate     = 16000
	bytesPerSample = 2 // s16le mono

	// maxPCMBuffer lets ffmpeg run ahead of a slow worker (about 17 min of audio), so
	// received_audio_sec and lag_sec stay honest when ASR falls behind.
	maxPCMBuffer = 32 << 20

	// failedAfter consecutive failed runs report asr.state=failed (retries continue)
	failedAfter = 5
	// healthyRun resets the restart backoff
	healthyRun = 30 * time.Second
	// maxDrainRestarts bounds restarts after the input has ended (finalize)
	maxDrainRestarts = 3
	// maxWorkerLine is the longest worker stdout line accepted
	maxWorkerLine = 4 << 20
)

// restartBackoff is the delay before each restart after a failed run
var restartBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// decoderArgs converts the growing WebM stream on stdin to 16 kHz mono s16le on stdout
var decoderArgs = []string{"-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-vn", "-ac", "1", "-ar", "16000", "-f", "s16le", "pipe:1"}

// WordTiming is one word with absolute session times
type WordTiming struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// workerEvent is one JSON line written by the live worker on stdout
type workerEvent struct {
	Type         string       `json:"type"`
	Text         string       `json:"text"`
	Start        float64      `json:"start"`
	End          float64      `json:"end"`
	Words        []WordTiming `json:"words"`
	ProcessedSec float64      `json:"processed_sec"`
	Engine       string       `json:"engine"`
	Message      string       `json:"message"`
}

// runResult describes how one ffmpeg (+ worker) run ended
type runResult struct {
	clean   bool  // input ended and everything was flushed
	upgrade bool  // decode-only run stopped because the ASR environment became ready
	healthy bool  // the worker reported ready
	err     error // why the run failed
}

// supervise runs the decode/ASR pipeline until the session input ends and is fully
// flushed, or the session context is cancelled. Failed runs are restarted with
// backoff; each restart re-decodes recording.webm from byte 0 and skips PCM before
// resume_at, so no text is duplicated and no WebM header is needed mid-stream.
func (s *session) supervise() {
	defer close(s.pipeDone)

	failures := 0
	drainRestarts := 0
	for {
		if s.ctx.Err() != nil {
			s.setASRState(asrStopped, "")
			return
		}
		if size, closed, _ := s.g.snapshot(); closed && size == 0 {
			s.setASRState(asrStopped, "") // the session ended without audio
			return
		}

		withWorker := s.m.env.isReady()
		resumeAt := s.beginRun(withWorker)
		start := time.Now()
		res := s.runOnce(withWorker, resumeAt)

		switch {
		case s.ctx.Err() != nil:
			s.setASRState(asrStopped, "")
			return
		case res.upgrade:
			logger.Info("Live ASR environment ready, starting worker", "job_id", s.id)
			continue
		case res.clean:
			s.setASRState(asrStopped, "")
			return
		}

		errMsg := "pipeline stopped unexpectedly"
		if res.err != nil {
			errMsg = res.err.Error()
		}
		logger.Warn("Live pipeline run failed", "job_id", s.id, "error", errMsg)

		if s.g.isClosed() {
			drainRestarts++
			if drainRestarts > maxDrainRestarts {
				s.setASRState(asrFailed, errMsg)
				return
			}
		}
		if res.healthy && time.Since(start) > healthyRun {
			failures = 0
		}
		failures++

		state := asrRestarting
		if failures >= failedAfter {
			state = asrFailed
		}
		s.asrMu.Lock()
		s.asr.restarts++
		s.asr.state = state
		s.asr.errMsg = errMsg
		s.asr.partial = nil
		s.asrMu.Unlock()

		delay := restartBackoff[len(restartBackoff)-1]
		if failures-1 < len(restartBackoff) {
			delay = restartBackoff[failures-1]
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-s.g.done: // stop pressed: drain right away
			timer.Stop()
		case <-s.ctx.Done():
			timer.Stop()
			s.setASRState(asrStopped, "")
			return
		}
	}
}

// beginRun resets per-run ASR stats and returns the resume offset
func (s *session) beginRun(withWorker bool) float64 {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	s.asr.partial = nil
	s.asr.processedSec = s.resumeAt
	if withWorker {
		if s.asr.state != asrRestarting && s.asr.state != asrFailed {
			s.asr.state = asrStarting
		}
	} else {
		s.asr.state = ""
	}
	s.touchLocked()
	return s.resumeAt
}

// runOnce starts ffmpeg (and the worker when withWorker) and waits until they exit.
func (s *session) runOnce(withWorker bool, resumeAt float64) runResult {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	logFile, err := os.OpenFile(filepath.Join(s.dir, workerLogFileName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return runResult{err: fmt.Errorf("failed to open worker log: %w", err)}
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n=== %s live pipeline run (resume_at=%.2fs, asr=%v)\n", time.Now().Format(time.RFC3339), resumeAt, withWorker)

	tail, err := openTail(filepath.Join(s.dir, recordingFileName), s.g)
	if err != nil {
		return runResult{err: fmt.Errorf("failed to open recording: %w", err)}
	}
	defer tail.Close()

	// Decoder: recording.webm (growing) -> 16 kHz mono s16le
	ffTail := &lineTail{}
	ff := s.m.decoderCommand(ctx)
	prepareCmd(ff)
	ffInR, ffInW, err := os.Pipe()
	if err != nil {
		return runResult{err: err}
	}
	ffOutR, ffOutW, err := os.Pipe()
	if err != nil {
		ffInR.Close()
		ffInW.Close()
		return runResult{err: err}
	}
	ff.Stdin = ffInR
	ff.Stdout = ffOutW
	ff.Stderr = io.MultiWriter(logFile, ffTail)
	if err := ff.Start(); err != nil {
		ffInR.Close()
		ffInW.Close()
		ffOutR.Close()
		ffOutW.Close()
		return runResult{err: fmt.Errorf("failed to start ffmpeg: %w", err)}
	}
	ffInR.Close()
	ffOutW.Close()

	go func() {
		_, _ = io.Copy(ffInW, tail)
		ffInW.Close()
	}()

	buf := newPCMBuffer(maxPCMBuffer)
	pumpDone := make(chan struct{})
	go func() {
		s.pumpPCM(ffOutR, buf, resumeAt)
		ffOutR.Close()
		close(pumpDone)
	}()

	ffCh := make(chan error, 1)
	go func() { ffCh <- ff.Wait() }()

	// Worker: PCM on stdin, JSON events on stdout
	var wkCh chan error
	var eventsDone chan struct{}
	var wkOutR *os.File
	wkTail := &lineTail{}
	run := &runEvents{}
	if withWorker {
		wk := s.m.workerCommand(ctx)
		prepareCmd(wk)
		wk.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
		wkInR, wkInW, err := os.Pipe()
		if err == nil {
			var wkOutW *os.File
			wkOutR, wkOutW, err = os.Pipe()
			if err == nil {
				wk.Stdin = wkInR
				wk.Stdout = wkOutW
				wk.Stderr = io.MultiWriter(logFile, wkTail)
				err = wk.Start()
				wkOutW.Close()
				if err != nil {
					wkOutR.Close()
				}
			}
			wkInR.Close()
			if err != nil {
				wkInW.Close()
			}
		}
		if err != nil {
			cancel()
			buf.abort()
			<-ffCh
			<-pumpDone
			return runResult{err: fmt.Errorf("failed to start live worker: %w", err)}
		}

		go func() {
			_, _ = io.Copy(wkInW, buf)
			wkInW.Close()
		}()
		eventsDone = make(chan struct{})
		go func() {
			s.readEvents(wkOutR, resumeAt, run, logFile)
			close(eventsDone)
		}()
		wkCh = make(chan error, 1)
		go func() { wkCh <- wk.Wait() }()
	} else {
		go func() { _, _ = io.Copy(io.Discard, buf) }()
	}

	var envReady <-chan struct{}
	if !withWorker {
		envReady = s.m.env.readyCh()
	}

	var ffErr, wkErr error
	ffDone, wkDone := false, !withWorker
	upgrade := false
	var grace <-chan time.Time
	for !ffDone || !wkDone {
		select {
		case ffErr = <-ffCh:
			ffDone = true
			tail.Close() // unblock the feeder
		case wkErr = <-wkCh:
			wkDone = true
			buf.abort()
			if !ffDone {
				select {
				case <-pumpDone:
					// ffmpeg already closed its output and is exiting on its own
					grace = time.After(5 * time.Second)
				default:
					cancel() // the worker is gone: stop the decoder too
				}
			}
		case <-grace:
			grace = nil
			cancel()
		case <-envReady:
			envReady = nil
			upgrade = true
			cancel()
		}
	}
	buf.abort()
	<-pumpDone
	if eventsDone != nil {
		select {
		case <-eventsDone:
		case <-time.After(5 * time.Second):
			wkOutR.Close() // a stray grandchild still holds stdout
			<-eventsDone
		}
	}

	res := runResult{upgrade: upgrade, healthy: run.gotReady()}
	if upgrade || s.ctx.Err() != nil {
		return res
	}
	inputEnded := s.g.isClosed()
	switch {
	case wkErr != nil:
		res.err = fmt.Errorf("live worker exited: %v%s", wkErr, detail(run.lastError(), wkTail.String()))
	case ffErr != nil && !inputEnded:
		res.err = fmt.Errorf("ffmpeg exited: %v%s", ffErr, detail("", ffTail.String()))
	case ffErr != nil:
		// The input is complete and the worker flushed: decoding the same bytes
		// again cannot do better (typically a truncated last frame).
		logger.Warn("ffmpeg reported an error at the end of the live recording", "job_id", s.id,
			"error", fmt.Sprintf("%v%s", ffErr, detail("", ffTail.String())))
		res.clean = true
	case !inputEnded:
		res.err = fmt.Errorf("decoder ended before the session input%s", detail("", ffTail.String()))
	default:
		res.clean = true
	}
	return res
}

// detail formats an optional error message and stderr tail for run errors
func detail(message, stderr string) string {
	if message != "" {
		return ": " + message
	}
	if stderr != "" {
		return ": " + stderr
	}
	return ""
}

// pumpPCM reads decoded PCM, counts received audio and forwards samples at or
// after resumeAt to buf. ffmpeg always decodes from byte 0, so the sample count
// is the absolute session time.
func (s *session) pumpPCM(r io.Reader, buf *pcmBuffer, resumeAt float64) {
	defer buf.closeWrite()
	skip := int64(resumeAt*sampleRate) * bytesPerSample
	var total int64
	chunk := make([]byte, 64*1024)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			data := chunk[:n]
			if total < skip {
				drop := skip - total
				if drop > int64(n) {
					drop = int64(n)
				}
				data = data[drop:]
			}
			total += int64(n)
			s.setReceived(float64(total/bytesPerSample) / sampleRate)
			if len(data) > 0 {
				if werr := buf.write(data); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// runEvents holds per-run worker state shared between goroutines
type runEvents struct {
	mu      sync.Mutex
	ready   bool
	lastErr string
}

func (r *runEvents) gotReady() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ready
}

func (r *runEvents) lastError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// readEvents parses the worker's JSON lines until stdout closes
func (s *session) readEvents(r *os.File, resumeAt float64, run *runEvents, logFile io.Writer) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxWorkerLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev workerEvent
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
			fmt.Fprintf(logFile, "worker stdout: %s\n", line)
			continue
		}
		s.handleEvent(ev, resumeAt, run)
	}
	if err := sc.Err(); err != nil {
		logger.Warn("Live worker output unreadable", "job_id", s.id, "error", err)
		_, _ = io.Copy(io.Discard, r) // keep the worker from blocking on a full pipe
	}
}

// handleEvent applies one worker event. Worker times are relative to the first
// sample it received, which is resumeAt in session time.
func (s *session) handleEvent(ev workerEvent, resumeAt float64, run *runEvents) {
	switch ev.Type {
	case "ready":
		run.mu.Lock()
		run.ready = true
		run.mu.Unlock()
		s.asrMu.Lock()
		s.asr.state = asrRunning
		s.asr.errMsg = ""
		if ev.Engine != "" {
			s.asr.engine = ev.Engine
		}
		s.touchLocked()
		s.asrMu.Unlock()

	case "partial":
		text := strings.TrimSpace(ev.Text)
		s.asrMu.Lock()
		if text == "" {
			s.asr.partial = nil
		} else {
			s.asr.partial = &Partial{Text: text, Start: round3(resumeAt + ev.Start)}
		}
		s.touchLocked()
		s.asrMu.Unlock()

	case "final":
		s.addFinal(ev, resumeAt)

	case "progress":
		s.asrMu.Lock()
		s.asr.processedSec = resumeAt + ev.ProcessedSec
		s.touchLocked()
		s.asrMu.Unlock()

	case "eof":
		s.asrMu.Lock()
		s.asr.partial = nil
		s.touchLocked()
		s.asrMu.Unlock()

	case "error":
		run.mu.Lock()
		run.lastErr = ev.Message
		run.mu.Unlock()
		s.asrMu.Lock()
		s.asr.errMsg = ev.Message
		s.touchLocked()
		s.asrMu.Unlock()
		logger.Warn("Live worker reported an error", "job_id", s.id, "error", ev.Message)
	}
}

// addFinal persists a final segment with absolute times and advances resume_at
func (s *session) addFinal(ev workerEvent, resumeAt float64) {
	text := strings.TrimSpace(ev.Text)
	start := resumeAt + ev.Start
	end := resumeAt + ev.End
	if end < start {
		end = start
	}

	var wordsJSON *string
	if len(ev.Words) > 0 {
		words := make([]WordTiming, 0, len(ev.Words))
		for _, w := range ev.Words {
			if word := strings.TrimSpace(w.Word); word != "" {
				words = append(words, WordTiming{Word: word, Start: round3(resumeAt + w.Start), End: round3(resumeAt + w.End)})
			}
		}
		if len(words) > 0 {
			if data, err := json.Marshal(words); err == nil {
				str := string(data)
				wordsJSON = &str
			}
		}
	}

	s.asrMu.Lock()
	s.asr.partial = nil
	if end > s.asr.processedSec {
		s.asr.processedSec = end
	}
	s.touchLocked()
	if text == "" {
		s.asrMu.Unlock()
		return
	}
	seg := models.LiveSegment{
		TranscriptionJobID: s.id,
		Seq:                s.nextSegSeq,
		StartSec:           round3(start),
		EndSec:             round3(end),
		Text:               text,
		WordsJSON:          wordsJSON,
	}
	s.nextSegSeq++
	s.segments = append(s.segments, seg)
	if end > s.resumeAt {
		s.resumeAt = end
	}
	s.asrMu.Unlock()

	// Persist outside the lock; the in-memory list stays authoritative for this
	// session, so a transient DB error never loses text before finalize.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = s.m.db.Create(&seg).Error; err == nil {
			return
		}
		time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
	}
	logger.Error("Failed to persist live segment", "job_id", s.id, "seq", seg.Seq, "error", err)
}

// decoderCommand builds the ffmpeg decode command (tests may replace it)
func (m *Manager) decoderCommand(ctx context.Context) *exec.Cmd {
	if len(m.decoderCmd) > 0 {
		return exec.CommandContext(ctx, m.decoderCmd[0], m.decoderCmd[1:]...)
	}
	return exec.CommandContext(ctx, m.ffmpeg, decoderArgs...)
}

// workerCommand builds the live worker command. LIVE_WORKER_CMD, when set, is a
// full command line used instead, with the same stdin/stdout protocol.
func (m *Manager) workerCommand(ctx context.Context) *exec.Cmd {
	if fields := strings.Fields(m.opts.WorkerCmd); len(fields) > 0 {
		return exec.CommandContext(ctx, fields[0], fields[1:]...)
	}
	return exec.CommandContext(ctx, "uv", "run", "--native-tls", "--project", m.opts.EnvDir,
		"python", filepath.Join(m.opts.EnvDir, "live_worker.py"),
		"--engine", m.opts.Engine,
		"--models-dir", m.opts.ModelsDir,
		"--threads", strconv.Itoa(m.opts.Threads))
}

// pcmBuffer is a bounded in-memory FIFO between ffmpeg and the worker
type pcmBuffer struct {
	mu      sync.Mutex
	cond    *sync.Cond
	chunks  [][]byte
	size    int
	max     int
	closedW bool
	aborted bool
}

var errBufferAborted = errors.New("pcm buffer aborted")

func newPCMBuffer(max int) *pcmBuffer {
	b := &pcmBuffer{max: max}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// write copies p into the buffer, blocking while it is full
func (b *pcmBuffer) write(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size >= b.max && !b.aborted {
		b.cond.Wait()
	}
	if b.aborted || b.closedW {
		return errBufferAborted
	}
	b.chunks = append(b.chunks, append([]byte(nil), p...))
	b.size += len(p)
	b.cond.Broadcast()
	return nil
}

// Read implements io.Reader: it returns io.EOF after closeWrite once drained
func (b *pcmBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.size == 0 && !b.closedW && !b.aborted {
		b.cond.Wait()
	}
	if b.aborted {
		return 0, errBufferAborted
	}
	if b.size == 0 {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && len(b.chunks) > 0 {
		c := copy(p[n:], b.chunks[0])
		n += c
		if c == len(b.chunks[0]) {
			b.chunks[0] = nil
			b.chunks = b.chunks[1:]
		} else {
			b.chunks[0] = b.chunks[0][c:]
		}
	}
	b.size -= n
	b.cond.Broadcast()
	return n, nil
}

// closeWrite ends the stream after the buffered data
func (b *pcmBuffer) closeWrite() {
	b.mu.Lock()
	b.closedW = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// abort discards buffered data and unblocks both sides
func (b *pcmBuffer) abort() {
	b.mu.Lock()
	b.aborted = true
	b.chunks = nil
	b.size = 0
	b.cond.Broadcast()
	b.mu.Unlock()
}

// lineTail keeps the last few hundred bytes written to it (stderr tails)
type lineTail struct {
	mu  sync.Mutex
	buf []byte
}

const lineTailMax = 512

func (t *lineTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > 4*lineTailMax {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-lineTailMax:]...)
	}
	return len(p), nil
}

// String returns the last non-empty lines, at most lineTailMax bytes
func (t *lineTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := lastLines(t.buf, 3)
	if len(out) > lineTailMax {
		out = out[len(out)-lineTailMax:]
	}
	return out
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

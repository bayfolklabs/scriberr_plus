package live

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"scriberr/internal/models"
	"scriberr/internal/transcription/interfaces"
	"scriberr/pkg/logger"
)

const (
	// drainTimeout bounds how long finalize waits for the pipeline without any
	// progress (worker event or decoded audio) before killing it
	drainTimeout = 120 * time.Second
	// maxDrain caps the total time finalize lets a ready worker work off its
	// backlog (twice the backlog at stop, at least drainTimeout). The live text is
	// a draft: a re-transcription covers whatever was not reached.
	maxDrain = 15 * time.Minute
	// modelLoadTimeout bounds the drain while the worker has not reported ready
	// (it may be downloading or extracting its model, which reports no progress)
	modelLoadTimeout = 30 * time.Minute
	remuxTimeout     = 10 * time.Minute
)

// finalize drains the pipeline, remuxes the recording, materializes the live
// transcript and completes the job. Recording files are never removed.
func (m *Manager) finalize(s *session) {
	defer m.wg.Done()
	logger.Info("Finalizing live session", "job_id", s.id)

	// 1. End the input: the tail reader returns EOF, ffmpeg and the worker flush.
	s.g.close()
	s.startPipeline() // no-op when already running; drains a reopened session
	if !m.waitDrained(s) {
		return // server shutting down: the session stays resumable
	}
	s.saveProgress()

	s.mu.Lock()
	closeErr := s.store.close()
	s.mu.Unlock()
	if closeErr != nil {
		// The recording may not be durable: the client must keep its copy.
		m.markFailed(s, fmt.Errorf("failed to flush live recording: %w", closeErr))
		return
	}

	// 2. Remux to a seekable file; fall back to the raw recording.
	ctx, cancel := context.WithTimeout(m.ctx, remuxTimeout)
	audioPath, err := m.remux(ctx, s.dir)
	cancel()
	if m.ctx.Err() != nil {
		return
	}
	if err != nil {
		logger.Warn("Live recording remux failed, keeping raw recording", "job_id", s.id, "error", err)
	}

	// 3. Materialize the live transcript.
	result := materialize(s.segmentsSnapshot(), s.engine())
	data, err := json.Marshal(result)
	if err != nil {
		m.markFailed(s, fmt.Errorf("failed to encode live transcript: %w", err))
		return
	}
	transcript := string(data)

	// 4. Complete the job, unless a batch pass owns its transcript (queued, running
	// or completed): then leave its status and transcript alone and only record
	// the live pass.
	var job models.TranscriptionJob
	if err := m.db.Select("id", "status", "transcript_source").First(&job, "id = ?", s.id).Error; err != nil {
		m.markFailed(s, fmt.Errorf("failed to load job: %w", err))
		return
	}
	updates := map[string]interface{}{
		"live_transcript": transcript,
		"live_status":     models.LiveStatusFinalized,
		"audio_path":      audioPath,
	}
	complete := job.Status == models.StatusUploaded || job.Status == models.StatusFailed ||
		(job.Status == models.StatusCompleted && job.TranscriptSource == models.TranscriptSourceLive)
	if complete {
		updates["transcript"] = transcript
		updates["transcript_source"] = models.TranscriptSourceLive
		updates["status"] = models.StatusCompleted
		updates["error_message"] = nil
	}
	if err := m.db.Model(&models.TranscriptionJob{}).Where("id = ?", s.id).Updates(updates).Error; err != nil {
		m.markFailed(s, fmt.Errorf("failed to save live transcript: %w", err))
		return
	}

	s.mu.Lock()
	s.status = models.LiveStatusFinalized
	s.mu.Unlock()
	m.removeSession(s)
	logger.Info("Live session finalized", "job_id", s.id, "segments", len(result.Segments), "audio_path", audioPath)

	// 5. Optional high-quality pass through the normal queue.
	if s.meta.AutoRetranscribe && complete {
		m.mu.Lock()
		retranscribe := m.retranscribe
		m.mu.Unlock()
		if retranscribe != nil {
			if err := retranscribe(m.ctx, s.id, s.meta.OwnerUserID); err != nil {
				logger.Warn("Failed to queue live session for re-transcription", "job_id", s.id, "error", err)
			}
		}
	}
}

// waitDrained waits for the pipeline to finish flushing. The pipeline is killed,
// and finalize goes on with the segments so far, when it makes no progress for
// drainTimeout, when a ready worker has not worked off its backlog within
// drainLimit, or when the worker has not become ready within modelLoadTimeout. It
// returns false when the server is shutting down.
func (m *Manager) waitDrained(s *session) bool {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	start := time.Now()
	limit := drainLimit(m.drainTimeout, m.maxDrain, s.backlog())
	since := start // the overall limit runs only while the worker is ready
	for {
		select {
		case <-s.pipeDone:
			return m.ctx.Err() == nil
		case <-m.ctx.Done():
			<-s.pipeDone
			return false
		case <-ticker.C:
			now := time.Now()
			reason := ""
			if s.workerLoading() {
				since = now
				if now.Sub(start) > m.modelLoadTimeout {
					reason = "worker did not load its model in time"
				}
			} else {
				last := s.lastActivity()
				if last.Before(start) {
					last = start
				}
				switch {
				case now.Sub(last) > m.drainTimeout:
					reason = "no progress"
				case now.Sub(since) > limit:
					reason = "backlog not worked off in time"
				}
			}
			if reason != "" {
				logger.Warn("Live pipeline did not flush in time, stopping it", "job_id", s.id, "reason", reason)
				s.cancel()
				<-s.pipeDone
				return m.ctx.Err() == nil
			}
		}
	}
}

// drainLimit is the overall time a ready worker gets to work off a backlog: twice
// the backlog, at least idle, at most max
func drainLimit(idle, max, backlog time.Duration) time.Duration {
	limit := 2 * backlog
	if limit < idle {
		limit = idle
	}
	if limit > max {
		limit = max
	}
	return limit
}

// remux copies recording.webm into a seekable recording.final.webm (duration and
// cues written). It returns the path the job should use for playback.
func (m *Manager) remux(ctx context.Context, dir string) (string, error) {
	src := filepath.Join(dir, recordingFileName)
	info, err := os.Stat(src)
	if err != nil {
		return src, err
	}
	if info.Size() == 0 {
		return src, fmt.Errorf("recording is empty")
	}

	dst := filepath.Join(dir, finalFileName)
	tmp := dst + ".part"
	cmd := exec.CommandContext(ctx, m.ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-i", src, "-c", "copy", "-f", "webm", tmp)
	prepareCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return src, fmt.Errorf("ffmpeg remux failed: %w: %s", err, lastLines(out, 3))
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		_ = os.Remove(tmp)
		return src, fmt.Errorf("ffmpeg remux produced no output")
	}
	// The job points at the result and the client then deletes its copy: make the
	// data durable before the rename can be.
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return src, fmt.Errorf("failed to flush remuxed recording: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return src, fmt.Errorf("failed to move remuxed recording: %w", err)
	}
	return dst, nil
}

// markFailed records a finalize failure; the recording files stay in place
func (m *Manager) markFailed(s *session, err error) {
	logger.Error("Live session finalize failed", "job_id", s.id, "error", err)
	msg := err.Error()
	if dbErr := m.db.Model(&models.TranscriptionJob{}).Where("id = ?", s.id).Updates(map[string]interface{}{
		"live_status":   models.LiveStatusFailed,
		"error_message": msg,
	}).Error; dbErr != nil {
		logger.Error("Failed to mark live session failed", "job_id", s.id, "error", dbErr)
	}
	s.mu.Lock()
	s.status = models.LiveStatusFailed
	_ = s.store.close()
	s.mu.Unlock()
	m.removeSession(s)
}

// materialize turns live segments into the TranscriptResult JSON the detail view
// reads. Words come from the worker when available; otherwise each segment's span
// is spread evenly over its words, so word_segments is never empty when there is
// text.
func materialize(segs []models.LiveSegment, engine string) interfaces.TranscriptResult {
	result := interfaces.TranscriptResult{
		Language:     "en",
		Segments:     []interfaces.TranscriptSegment{},
		WordSegments: []interfaces.TranscriptWord{},
		ModelUsed:    "live:" + engine,
		Metadata:     map[string]string{"source": models.SourceLive, "engine": engine},
	}

	texts := make([]string, 0, len(segs))
	for _, seg := range segs {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			continue
		}
		start, end := seg.StartSec, seg.EndSec
		if end < start {
			end = start
		}
		texts = append(texts, text)
		result.Segments = append(result.Segments, interfaces.TranscriptSegment{Start: start, End: end, Text: text})
		result.WordSegments = append(result.WordSegments, segmentWords(seg, text, start, end)...)
	}
	result.Text = strings.Join(texts, " ")
	return result
}

// segmentWords returns the worker's word timings, or an even split of the span
func segmentWords(seg models.LiveSegment, text string, start, end float64) []interfaces.TranscriptWord {
	if seg.WordsJSON != nil && *seg.WordsJSON != "" {
		var timed []WordTiming
		if err := json.Unmarshal([]byte(*seg.WordsJSON), &timed); err == nil {
			words := make([]interfaces.TranscriptWord, 0, len(timed))
			for _, w := range timed {
				if word := strings.TrimSpace(w.Word); word != "" {
					wEnd := w.End
					if wEnd < w.Start {
						wEnd = w.Start
					}
					words = append(words, interfaces.TranscriptWord{Word: word, Start: w.Start, End: wEnd, Score: 1})
				}
			}
			if len(words) > 0 {
				return words
			}
		}
	}

	fields := strings.Fields(text)
	words := make([]interfaces.TranscriptWord, 0, len(fields))
	step := (end - start) / float64(len(fields))
	for i, field := range fields {
		words = append(words, interfaces.TranscriptWord{
			Word:  field,
			Start: round3(start + float64(i)*step),
			End:   round3(start + float64(i+1)*step),
		})
	}
	return words
}

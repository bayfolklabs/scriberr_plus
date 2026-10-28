package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"scriberr/internal/live"
	"scriberr/internal/models"
	"scriberr/pkg/logger"

	"github.com/gin-gonic/gin"
)

// CreateLiveSessionRequest represents the create live session request
type CreateLiveSessionRequest struct {
	Title            string `json:"title"`
	MimeType         string `json:"mime_type"`
	AutoRetranscribe bool   `json:"auto_retranscribe"`
}

// StopLiveSessionRequest represents the stop live session request
type StopLiveSessionRequest struct {
	LastSeq *int `json:"last_seq"` // highest chunk seq the client recorded
}

// @Summary Create live session
// @Description Create a live transcription session (a transcription job with source=live) and start its ASR pipeline
// @Tags live
// @Accept json
// @Produce json
// @Param request body CreateLiveSessionRequest false "Session settings"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/v1/live/sessions [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) CreateLiveSession(c *gin.Context) {
	var req CreateLiveSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": err.Error()})
		return
	}

	var owner *uint
	if userID, exists := c.Get("user_id"); exists {
		if id, ok := userID.(uint); ok {
			owner = &id
		}
	}

	job, err := h.liveManager.CreateSession(c.Request.Context(), live.CreateRequest{
		Title:            req.Title,
		MimeType:         req.MimeType,
		AutoRetranscribe: req.AutoRetranscribe,
		OwnerUserID:      owner,
	})
	if err != nil {
		logger.Error("Failed to create live session", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create live session"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"id": job.ID, "chunk_ms": live.ChunkMillis})
}

// @Summary Upload live audio chunk
// @Description Append chunk seq (raw MediaRecorder bytes, up to 8 MiB) to a live session. Duplicates are acknowledged; gaps return 409 with next_seq.
// @Tags live
// @Accept octet-stream
// @Produce json
// @Param id path string true "Session (job) ID"
// @Param seq query int true "Chunk sequence number, starting at 0"
// @Success 200 {object} live.ChunkAck
// @Failure 400 {object} map[string]interface{}
// @Failure 404 {object} map[string]interface{}
// @Failure 409 {object} map[string]interface{}
// @Failure 413 {object} map[string]interface{}
// @Router /api/v1/live/sessions/{id}/chunks [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) AppendLiveChunk(c *gin.Context) {
	seq, err := strconv.Atoi(c.Query("seq"))
	if err != nil || seq < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": "seq query parameter must be a non-negative integer"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, live.MaxChunkBytes)
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "too_large", "message": fmt.Sprintf("chunk exceeds %d bytes", live.MaxChunkBytes)})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": "failed to read chunk body"})
		return
	}

	ack, err := h.liveManager.AppendChunk(c.Request.Context(), c.Param("id"), seq, data)
	if err != nil {
		h.writeLiveError(c, err)
		return
	}
	c.JSON(http.StatusOK, ack)
}

// @Summary Get live session state
// @Description Live status, upload progress, ASR progress, final segments with seq > after, and the current partial
// @Tags live
// @Produce json
// @Param id path string true "Session (job) ID"
// @Param after query int false "Return only segments with seq greater than this (-1 for all)" default(-1)
// @Success 200 {object} live.State
// @Failure 404 {object} map[string]interface{}
// @Router /api/v1/live/sessions/{id}/state [get]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) GetLiveSessionState(c *gin.Context) {
	after, err := strconv.Atoi(c.DefaultQuery("after", "-1"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": "after must be an integer"})
		return
	}

	state, err := h.liveManager.State(c.Request.Context(), c.Param("id"), after)
	if err != nil {
		h.writeLiveError(c, err)
		return
	}
	c.JSON(http.StatusOK, state)
}

// @Summary Stop live session
// @Description Stop recording and finalize asynchronously. Returns 409 missing_chunks when the server has not received every chunk up to last_seq.
// @Tags live
// @Accept json
// @Produce json
// @Param id path string true "Session (job) ID"
// @Param request body StopLiveSessionRequest false "Highest recorded chunk seq"
// @Success 202 {object} map[string]string
// @Failure 404 {object} map[string]interface{}
// @Failure 409 {object} map[string]interface{}
// @Router /api/v1/live/sessions/{id}/stop [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) StopLiveSession(c *gin.Context) {
	var req StopLiveSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": err.Error()})
		return
	}
	lastSeq := -1
	if req.LastSeq != nil {
		lastSeq = *req.LastSeq
	}

	status, err := h.liveManager.Stop(c.Request.Context(), c.Param("id"), lastSeq)
	if err != nil {
		h.writeLiveError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"live_status": status})
}

// @Summary Finalize live session
// @Description Finalize a session without the client (e.g. interrupted after a crash), using whatever chunks arrived
// @Tags live
// @Produce json
// @Param id path string true "Session (job) ID"
// @Success 202 {object} map[string]string
// @Failure 404 {object} map[string]interface{}
// @Failure 409 {object} map[string]interface{}
// @Router /api/v1/live/sessions/{id}/finalize [post]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) FinalizeLiveSession(c *gin.Context) {
	status, err := h.liveManager.Finalize(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.writeLiveError(c, err)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"live_status": status})
}

// @Summary List live sessions
// @Description List live sessions by live status (default: recording,interrupted) for the recovery UI
// @Tags live
// @Produce json
// @Param status query string false "Comma-separated live statuses" default(recording,interrupted)
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]interface{}
// @Router /api/v1/live/sessions [get]
// @Security ApiKeyAuth
// @Security BearerAuth
func (h *Handler) ListLiveSessions(c *gin.Context) {
	var statuses []string
	for _, st := range strings.Split(c.Query("status"), ",") {
		if st = strings.TrimSpace(st); st != "" {
			statuses = append(statuses, st)
		}
	}

	sessions, err := h.liveManager.List(c.Request.Context(), statuses)
	if err != nil {
		h.writeLiveError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

// writeLiveError maps live manager errors to the API's JSON error shapes
func (h *Handler) writeLiveError(c *gin.Context, err error) {
	var liveErr *live.Error
	if !errors.As(err, &liveErr) {
		logger.Error("Live session request failed", "path", c.Request.URL.Path, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal_error", "message": err.Error()})
		return
	}

	switch liveErr.Code {
	case live.CodeNotFound:
		c.JSON(http.StatusNotFound, gin.H{"error": live.CodeNotFound, "message": "Live session not found"})
	case live.CodeInvalid:
		c.JSON(http.StatusBadRequest, gin.H{"error": live.CodeInvalid, "message": liveErr.Message})
	case live.CodeGap, live.CodeMissingChunks:
		c.JSON(http.StatusConflict, gin.H{"error": liveErr.Code, "next_seq": liveErr.NextSeq})
	case live.CodeClosed:
		c.JSON(http.StatusConflict, gin.H{"error": live.CodeClosed, "live_status": liveErr.LiveStatus})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": liveErr.Code, "message": liveErr.Message})
	}
}

// enqueueLiveRetranscription queues a finalized live session for the normal
// (high-quality) pipeline, the same way POST /transcription/:id/start does, using
// the owner's default profile, else the system default profile, else the
// defaults StartTranscription uses. The live transcript stays in LiveTranscript.
func (h *Handler) enqueueLiveRetranscription(ctx context.Context, jobID string, ownerUserID *uint) error {
	job, err := h.jobRepo.FindByID(ctx, jobID)
	if err != nil {
		return fmt.Errorf("failed to load job: %w", err)
	}
	if job.Status != models.StatusUploaded && job.Status != models.StatusCompleted && job.Status != models.StatusFailed {
		return nil // already queued or running
	}

	var profile *models.TranscriptionProfile
	if ownerUserID != nil {
		if user, err := h.userService.GetUser(ctx, *ownerUserID); err == nil && user.DefaultProfileID != nil {
			profile, _ = h.profileRepo.FindByID(ctx, *user.DefaultProfileID)
		}
	}
	if profile == nil {
		profile, _ = h.profileRepo.FindDefault(ctx)
	}
	params := defaultRetranscriptionParams()
	if profile != nil {
		params = profile.Parameters
	}
	params.IsMultiTrackEnabled = false // a live recording is always single-track

	job.Parameters = params
	job.Diarization = params.Diarize
	job.Status = models.StatusPending
	job.Transcript = nil
	job.Summary = nil
	job.ErrorMessage = nil
	job.TranscriptSource = models.TranscriptSourceBatch
	if err := h.jobRepo.Update(ctx, job); err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}

	if err := h.taskQueue.EnqueueJob(jobID); err != nil {
		// Do not leave the meeting pending and empty: restore the live transcript.
		job.Status = models.StatusCompleted
		job.Transcript = job.LiveTranscript
		job.TranscriptSource = models.TranscriptSourceLive
		_ = h.jobRepo.Update(ctx, job)
		return fmt.Errorf("failed to enqueue job: %w", err)
	}

	logger.Info("Live session queued for re-transcription", "job_id", jobID,
		"model_family", params.ModelFamily, "model", params.Model)
	return nil
}

// defaultRetranscriptionParams mirrors the defaults of getValidatedTranscriptionParams
// (POST /transcription/:id/start with an empty body).
func defaultRetranscriptionParams() models.WhisperXParams {
	return models.WhisperXParams{
		ModelFamily:                    "whisper",
		Model:                          "small",
		Device:                         "cpu",
		BatchSize:                      8,
		ComputeType:                    "float32",
		OutputFormat:                   "all",
		Verbose:                        true,
		Task:                           "transcribe",
		InterpolateMethod:              "nearest",
		VadMethod:                      "pyannote",
		VadOnset:                       0.5,
		VadOffset:                      0.363,
		ChunkSize:                      30,
		DiarizeModel:                   "pyannote/speaker-diarization-3.1",
		BestOf:                         5,
		BeamSize:                       5,
		Patience:                       1.0,
		LengthPenalty:                  1.0,
		Fp16:                           true,
		TemperatureIncrementOnFallback: 0.2,
		CompressionRatioThreshold:      2.4,
		LogprobThreshold:               -1.0,
		NoSpeechThreshold:              0.6,
		SegmentResolution:              "sentence",
		AttentionContextLeft:           256,
		AttentionContextRight:          256,
	}
}

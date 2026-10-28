// Package live implements live meeting sessions: audio chunks are appended to a
// durable recording on disk, while a supervised ffmpeg + ASR worker pipeline
// consumes that file and produces a draft transcript as the meeting happens.
// See LIVE_DESIGN.md for the contract.
package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"scriberr/internal/config"
	"scriberr/internal/models"
	"scriberr/internal/transcription/adapters"
	"scriberr/pkg/logger"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// ChunkMillis is the MediaRecorder timeslice clients should use
	ChunkMillis = 1000
	// MaxChunkBytes caps one chunk upload
	MaxChunkBytes = 8 << 20

	janitorInterval = time.Minute
)

// Error codes returned by the manager (the HTTP layer maps them to statuses)
const (
	CodeNotFound      = "not_found"
	CodeInvalid       = "invalid_request"
	CodeGap           = "gap"
	CodeClosed        = "closed"
	CodeMissingChunks = "missing_chunks"
)

// Error is a live session error with the details the API reports
type Error struct {
	Code       string
	NextSeq    int
	LiveStatus string
	Message    string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// RetranscribeFunc queues the normal batch pipeline for a finalized live job
type RetranscribeFunc func(ctx context.Context, jobID string, ownerUserID *uint) error

// Options configures the manager
type Options struct {
	UploadDir    string        // sessions live in <UploadDir>/live/<job_id>/
	EnvDir       string        // <WHISPERX_ENV>/live (made absolute)
	Engine       string        // LIVE_ENGINE
	Threads      int           // LIVE_THREADS
	ModelsDir    string        // LIVE_MODELS_DIR (made absolute)
	AbandonAfter time.Duration // LIVE_ABANDON_MINUTES
	WorkerCmd    string        // LIVE_WORKER_CMD: replaces the uv worker command line
	WorkerScript []byte        // embedded live_worker.py
	Pyproject    []byte        // embedded pyproject.toml
}

// OptionsFromConfig builds manager options from the server configuration
func OptionsFromConfig(cfg *config.Config) Options {
	return Options{
		UploadDir:    cfg.UploadDir,
		EnvDir:       filepath.Join(cfg.WhisperXEnv, "live"),
		Engine:       cfg.LiveEngine,
		Threads:      cfg.LiveThreads,
		ModelsDir:    cfg.LiveModelsDir,
		AbandonAfter: time.Duration(cfg.LiveAbandonMinutes) * time.Minute,
		WorkerCmd:    cfg.LiveWorkerCmd,
		WorkerScript: adapters.LiveWorkerScript,
		Pyproject:    adapters.LivePyproject,
	}
}

// CreateRequest describes a new live session
type CreateRequest struct {
	Title            string
	MimeType         string
	AutoRetranscribe bool
	OwnerUserID      *uint
}

// ChunkAck is the result of a chunk upload
type ChunkAck struct {
	Ack       int  `json:"ack"`
	NextSeq   int  `json:"next_seq"`
	Duplicate bool `json:"duplicate,omitempty"`
}

// ASRState is the ASR part of the session state
type ASRState struct {
	State        string  `json:"state"`
	ProcessedSec float64 `json:"processed_sec"`
	LagSec       float64 `json:"lag_sec"`
	Engine       string  `json:"engine"`
	Restarts     int     `json:"restarts"`
	Error        string  `json:"error"`
}

// Segment is a final live segment as reported by GET /state
type Segment struct {
	Seq   int     `json:"seq"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Partial is the current unfinished hypothesis (memory only)
type Partial struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
}

// State is the response of GET /api/v1/live/sessions/:id/state
type State struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	LiveStatus       string    `json:"live_status"`
	Status           string    `json:"status"`
	NextSeq          int       `json:"next_seq"`
	Bytes            int64     `json:"bytes"`
	ReceivedAudioSec float64   `json:"received_audio_sec"`
	ASR              ASRState  `json:"asr"`
	Segments         []Segment `json:"segments"`
	Partial          *Partial  `json:"partial"`
	AutoRetranscribe bool      `json:"auto_retranscribe"`
}

// Summary is one entry of GET /api/v1/live/sessions
type Summary struct {
	ID               string    `json:"id"`
	Title            string    `json:"title"`
	LiveStatus       string    `json:"live_status"`
	Status           string    `json:"status"`
	NextSeq          int       `json:"next_seq"`
	Bytes            int64     `json:"bytes"`
	AutoRetranscribe bool      `json:"auto_retranscribe"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// sessionMeta is persisted as session.json so settings survive restarts
type sessionMeta struct {
	Title            string    `json:"title"`
	MimeType         string    `json:"mime_type"`
	AutoRetranscribe bool      `json:"auto_retranscribe"`
	OwnerUserID      *uint     `json:"owner_user_id,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ReceivedAudioSec float64   `json:"received_audio_sec,omitempty"` // saved when the session closes
	LastSeq          *int      `json:"last_seq,omitempty"`           // highest seq the client recorded, saved at stop
}

// asrStats is the live ASR progress of a session
type asrStats struct {
	state        string // "" while decoding without a worker (environment not ready)
	engine       string
	errMsg       string
	restarts     int
	receivedSec  float64
	processedSec float64
	partial      *Partial
}

// session is an open live session: its chunk store and its pipeline
type session struct {
	m    *Manager
	id   string
	dir  string
	meta sessionMeta

	mu          sync.Mutex // guards store, status, lastChunkAt, meta.LastSeq and session.json writes
	store       *chunkStore
	status      string
	lastChunkAt time.Time

	g *growth

	asrMu      sync.Mutex // guards the fields below
	asr        asrStats
	segments   []models.LiveSegment // all finals, in seq order
	nextSegSeq int
	resumeAt   float64 // end of the last final segment
	activity   time.Time
	run        *runEvents // the run with a worker in progress, nil when none

	ctx         context.Context
	cancel      context.CancelFunc
	pipeOnce    sync.Once
	pipeStarted bool
	pipeDone    chan struct{}
}

// Manager owns all open live sessions
type Manager struct {
	db   *gorm.DB
	opts Options
	env  *pythonEnv

	ffmpeg           string
	decoderCmd       []string // tests: replaces the ffmpeg decode command
	drainTimeout     time.Duration
	maxDrain         time.Duration
	modelLoadTimeout time.Duration
	janitorInterval  time.Duration

	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time

	mu              sync.Mutex
	sessions        map[string]*session // every open session is recording or finalizing
	retranscribe    RetranscribeFunc
	pendingFinalize []string // finalizing when the server stopped; resumed one at a time

	wg sync.WaitGroup // janitor, finalizes and environment preparation
}

// errBusy is returned by a background finalize while a session is open
var errBusy = errors.New("a live session is recording or finalizing")

// NewManager creates a live session manager. Call Recover, then Start.
func NewManager(db *gorm.DB, opts Options) *Manager {
	if opts.Engine == "" {
		opts.Engine = "nemotron-en-1120ms"
	}
	if opts.Threads <= 0 {
		opts.Threads = 4
	}
	if opts.AbandonAfter <= 0 {
		opts.AbandonAfter = 30 * time.Minute
	}
	// uv runs the worker with its own working directory, so pass absolute paths.
	if abs, err := filepath.Abs(opts.EnvDir); err == nil {
		opts.EnvDir = abs
	}
	if abs, err := filepath.Abs(opts.ModelsDir); err == nil {
		opts.ModelsDir = abs
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		db:               db,
		opts:             opts,
		env:              newPythonEnv(opts.EnvDir, opts.WorkerScript, opts.Pyproject, strings.TrimSpace(opts.WorkerCmd) != ""),
		ffmpeg:           "ffmpeg",
		drainTimeout:     drainTimeout,
		maxDrain:         maxDrain,
		modelLoadTimeout: modelLoadTimeout,
		janitorInterval:  janitorInterval,
		ctx:              ctx,
		cancel:           cancel,
		startedAt:        time.Now(),
		sessions:         make(map[string]*session),
	}
}

// SetRetranscriber sets the function used for auto re-transcription
func (m *Manager) SetRetranscriber(fn RetranscribeFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retranscribe = fn
}

// Recover marks sessions that were recording when the server stopped as
// interrupted: they wait for the client to resume, or the janitor. Sessions that
// were finalizing are finalized again by Start.
func (m *Manager) Recover(ctx context.Context) error {
	var finalizing []string
	if err := m.db.WithContext(ctx).Model(&models.TranscriptionJob{}).
		Where("source = ? AND live_status = ?", models.SourceLive, models.LiveStatusFinalizing).
		Pluck("id", &finalizing).Error; err != nil {
		return fmt.Errorf("failed to recover live sessions: %w", err)
	}
	m.mu.Lock()
	m.pendingFinalize = finalizing
	m.mu.Unlock()

	res := m.db.WithContext(ctx).Model(&models.TranscriptionJob{}).
		Where("source = ? AND live_status = ?", models.SourceLive, models.LiveStatusRecording).
		Update("live_status", models.LiveStatusInterrupted)
	if res.Error != nil {
		return fmt.Errorf("failed to recover live sessions: %w", res.Error)
	}
	if res.RowsAffected > 0 {
		logger.Info("Live sessions interrupted by restart are resumable", "count", res.RowsAffected)
	}
	return nil
}

// PrepareEnvironment starts preparing the live Python environment in the
// background. It never blocks; sessions record without ASR until it is ready.
func (m *Manager) PrepareEnvironment() {
	if strings.TrimSpace(m.opts.WorkerCmd) != "" {
		logger.Info("Live worker command override active", "LIVE_WORKER_CMD", m.opts.WorkerCmd)
		return
	}
	m.ensureEnv()
}

// ensureEnv starts environment preparation (if needed) as a manager goroutine, so
// Shutdown waits until its uv commands are killed. Checking for shutdown under
// m.mu keeps the WaitGroup Add ordered before Shutdown's Wait.
func (m *Manager) ensureEnv() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return
	}
	m.env.ensure(m.ctx, &m.wg)
}

// Start resumes the first finalization cut off by a restart (the janitor resumes
// the others, one at a time) and starts the janitor. It also starts environment
// preparation if PrepareEnvironment was not called.
func (m *Manager) Start() {
	m.ensureEnv()
	m.resumePending()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.janitorInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.sweep()
			case <-m.ctx.Done():
				return
			}
		}
	}()
}

// Shutdown stops all pipelines (killing ffmpeg and worker process trees) and
// fsyncs the recordings. Sessions stay 'recording' in the database, so the next
// startup marks them interrupted and they remain resumable.
func (m *Manager) Shutdown(ctx context.Context) {
	m.cancel()

	m.mu.Lock()
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	for _, s := range sessions {
		s.waitPipeline(ctx)
		s.saveProgress()
		s.mu.Lock()
		if err := s.store.close(); err != nil {
			logger.Warn("Failed to close live recording", "job_id", s.id, "error", err)
		}
		s.mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		logger.Warn("Timed out waiting for live sessions to stop")
	}
}

// CreateSession creates the job row and session directory and starts the
// pipeline, which waits for audio.
func (m *Manager) CreateSession(ctx context.Context, req CreateRequest) (*models.TranscriptionJob, error) {
	if m.ctx.Err() != nil {
		return nil, fmt.Errorf("server is shutting down")
	}

	id := uuid.New().String()
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = "Live session " + time.Now().Format("2006-01-02 15:04")
	}
	dir := filepath.Join(m.opts.UploadDir, "live", id)

	store, err := openChunkStore(dir)
	if err != nil {
		return nil, err
	}
	meta := sessionMeta{
		Title:            title,
		MimeType:         req.MimeType,
		AutoRetranscribe: req.AutoRetranscribe,
		OwnerUserID:      req.OwnerUserID,
		CreatedAt:        time.Now(),
	}
	if err := writeMeta(dir, meta); err != nil {
		_ = store.close()
		_ = os.RemoveAll(dir)
		return nil, err
	}

	recPath := store.recordingPath()
	job := models.TranscriptionJob{
		ID:            id,
		Title:         &title,
		Status:        models.StatusUploaded,
		AudioPath:     recPath,
		Source:        models.SourceLive,
		LiveStatus:    models.LiveStatusRecording,
		RecordingPath: &recPath,
	}
	if err := m.db.WithContext(ctx).Create(&job).Error; err != nil {
		_ = store.close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("failed to create live job: %w", err)
	}

	s := m.newSession(id, dir, meta, store, nil)
	s.status = models.LiveStatusRecording
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	s.startPipeline()
	m.ensureEnv() // retry a failed environment setup

	logger.Info("Live session started", "job_id", id, "title", title)
	return &job, nil
}

// AppendChunk appends chunk seq of a session. An interrupted session is reopened.
func (m *Manager) AppendChunk(ctx context.Context, id string, seq int, data []byte) (ChunkAck, error) {
	if seq < 0 {
		return ChunkAck{}, &Error{Code: CodeInvalid, Message: "seq must be >= 0"}
	}
	for attempt := 0; ; attempt++ {
		s, err := m.openForChunks(ctx, id)
		if err != nil {
			return ChunkAck{}, err
		}

		s.mu.Lock()
		if s.status != models.LiveStatusRecording {
			status := s.status
			s.mu.Unlock()
			if status == models.LiveStatusInterrupted && attempt == 0 {
				continue // suspended by the janitor meanwhile: reopen
			}
			return ChunkAck{}, &Error{Code: CodeClosed, LiveStatus: status}
		}

		err = s.store.append(seq, data)
		next := s.store.nextSeq
		s.g.set(s.store.size)
		s.lastChunkAt = time.Now()
		s.mu.Unlock()

		switch {
		case errors.Is(err, errDuplicateChunk):
			return ChunkAck{Ack: seq, NextSeq: next, Duplicate: true}, nil
		case errors.Is(err, errChunkGap):
			return ChunkAck{}, &Error{Code: CodeGap, NextSeq: next}
		case err != nil:
			return ChunkAck{}, err
		}
		return ChunkAck{Ack: seq, NextSeq: next}, nil
	}
}

// openForChunks returns the open session, reopening an interrupted one
func (m *Manager) openForChunks(ctx context.Context, id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[id]; s != nil {
		return s, nil
	}
	if m.ctx.Err() != nil {
		return nil, fmt.Errorf("server is shutting down")
	}

	job, err := m.findLiveJob(ctx, id)
	if err != nil {
		return nil, err
	}
	// 'recording' without an open session means recovery did not run; treat it
	// like 'interrupted'.
	if job.LiveStatus != models.LiveStatusInterrupted && job.LiveStatus != models.LiveStatusRecording {
		return nil, &Error{Code: CodeClosed, LiveStatus: job.LiveStatus}
	}

	s, err := m.loadSession(ctx, job)
	if err != nil {
		return nil, err
	}
	if err := m.setLiveStatus(ctx, id, models.LiveStatusRecording); err != nil {
		_ = s.store.close()
		return nil, err
	}
	s.status = models.LiveStatusRecording
	m.sessions[id] = s
	s.startPipeline()
	logger.Info("Live session reopened", "job_id", id, "next_seq", s.store.nextSeq, "resume_at", s.resumeAt)
	return s, nil
}

// State returns the session state with segments whose seq is greater than after
func (m *Manager) State(ctx context.Context, id string, after int) (*State, error) {
	job, err := m.findLiveJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if s := m.get(id); s != nil {
		return s.snapshot(job, after), nil
	}
	return m.stateFromDisk(ctx, job, after)
}

// Stop finalizes a session after checking that the server has every chunk up to
// lastSeq. It returns the resulting live status.
func (m *Manager) Stop(ctx context.Context, id string, lastSeq int) (string, error) {
	return m.close(ctx, id, &lastSeq, false)
}

// Finalize finalizes a session without the client, using whatever chunks arrived.
func (m *Manager) Finalize(ctx context.Context, id string) (string, error) {
	return m.close(ctx, id, nil, false)
}

// close finalizes a session. A background close (janitor, resumed finalize) never
// touches an open session: it returns errBusy while any session is open, so
// background finalizes run one at a time and never while a meeting is live.
func (m *Manager) close(ctx context.Context, id string, lastSeq *int, background bool) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		m.mu.Lock()
		if background && len(m.sessions) > 0 {
			m.mu.Unlock()
			return "", errBusy
		}
		s := m.sessions[id]
		if s == nil {
			return m.closeFromDisk(ctx, id, lastSeq, background) // unlocks m.mu
		}
		m.mu.Unlock()

		s.mu.Lock()
		switch s.status {
		case models.LiveStatusFinalizing, models.LiveStatusFinalized:
			status := s.status
			s.mu.Unlock()
			return status, nil
		case models.LiveStatusInterrupted, models.LiveStatusFailed:
			// A janitor suspend (or a failed finalize) is removing the session. Once
			// it is gone the next pass finalizes it from disk.
			s.mu.Unlock()
			continue
		}
		if lastSeq != nil && s.store.nextSeq <= *lastSeq {
			next := s.store.nextSeq
			s.mu.Unlock()
			return "", &Error{Code: CodeMissingChunks, NextSeq: next}
		}
		// Every acknowledged chunk is durable before the stop is accepted.
		if err := s.store.sync(); err != nil {
			s.mu.Unlock()
			return "", err
		}
		s.status = models.LiveStatusFinalizing
		s.setLastSeqLocked(lastSeq)
		s.mu.Unlock()
		if lastSeq != nil {
			s.saveProgress()
		}
		return m.beginFinalize(ctx, s)
	}
}

// closeFromDisk finalizes a session that is not open. It is called with m.mu held,
// which it releases; holding it while the session is loaded keeps a chunk from
// reopening the session meanwhile.
func (m *Manager) closeFromDisk(ctx context.Context, id string, lastSeq *int, background bool) (string, error) {
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("server is shutting down")
	}
	job, err := m.findLiveJob(ctx, id)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	switch job.LiveStatus {
	case models.LiveStatusFinalized:
		m.mu.Unlock()
		return job.LiveStatus, nil
	case models.LiveStatusRecording, models.LiveStatusInterrupted, models.LiveStatusFinalizing, models.LiveStatusFailed:
	default:
		m.mu.Unlock()
		return "", &Error{Code: CodeClosed, LiveStatus: job.LiveStatus}
	}
	if background && job.LiveStatus != models.LiveStatusInterrupted && job.LiveStatus != models.LiveStatusFinalizing {
		m.mu.Unlock()
		return job.LiveStatus, nil
	}
	if lastSeq != nil {
		if next, _, err := indexSummary(m.jobDir(job)); err == nil && next <= *lastSeq {
			m.mu.Unlock()
			return "", &Error{Code: CodeMissingChunks, NextSeq: next}
		}
	}
	s, err := m.loadSession(ctx, job)
	if err != nil {
		m.mu.Unlock()
		return "", err
	}

	// Check again on what the store kept. A finalize resumed after a restart checks
	// the last seq its client reported at stop: acknowledged chunks lost at a power
	// loss make the session resumable again, so the client can upload them.
	check, resumed := lastSeq, lastSeq == nil && job.LiveStatus == models.LiveStatusFinalizing
	if resumed {
		check = s.meta.LastSeq
	}
	if check != nil && s.store.nextSeq <= *check {
		next := s.store.nextSeq
		_ = s.store.close()
		if !resumed {
			m.mu.Unlock()
			return "", &Error{Code: CodeMissingChunks, NextSeq: next}
		}
		err := m.setLiveStatus(ctx, id, models.LiveStatusInterrupted)
		m.mu.Unlock()
		if err != nil {
			return "", err
		}
		logger.Warn("Live session lost chunks the client sent before stop; waiting for the client to resume",
			"job_id", id, "next_seq", next, "last_seq", *check)
		return models.LiveStatusInterrupted, nil
	}

	s.status = models.LiveStatusFinalizing
	s.setLastSeqLocked(lastSeq)
	m.sessions[id] = s
	m.mu.Unlock()
	if lastSeq != nil {
		s.saveProgress()
	}
	return m.beginFinalize(ctx, s)
}

// beginFinalize records the finalizing state and runs finalize asynchronously
func (m *Manager) beginFinalize(ctx context.Context, s *session) (string, error) {
	if err := m.setLiveStatus(ctx, s.id, models.LiveStatusFinalizing); err != nil {
		logger.Warn("Failed to record finalizing state", "job_id", s.id, "error", err)
	}
	m.wg.Add(1)
	go m.finalize(s)
	return models.LiveStatusFinalizing, nil
}

// liveStatuses are the statuses accepted by List
var liveStatuses = map[string]bool{
	models.LiveStatusRecording:   true,
	models.LiveStatusInterrupted: true,
	models.LiveStatusFinalizing:  true,
	models.LiveStatusFinalized:   true,
	models.LiveStatusFailed:      true,
}

// List returns live sessions with the given statuses (default: recording and
// interrupted), newest first.
func (m *Manager) List(ctx context.Context, statuses []string) ([]Summary, error) {
	if len(statuses) == 0 {
		statuses = []string{models.LiveStatusRecording, models.LiveStatusInterrupted}
	}
	for _, st := range statuses {
		if !liveStatuses[st] {
			return nil, &Error{Code: CodeInvalid, Message: "unknown live status: " + st}
		}
	}

	var jobs []models.TranscriptionJob
	if err := m.db.WithContext(ctx).
		Select("id", "title", "status", "live_status", "recording_path", "created_at", "updated_at").
		Where("source = ? AND live_status IN ?", models.SourceLive, statuses).
		Order("created_at desc").
		Find(&jobs).Error; err != nil {
		return nil, fmt.Errorf("failed to list live sessions: %w", err)
	}

	out := make([]Summary, 0, len(jobs))
	for i := range jobs {
		job := &jobs[i]
		dir := m.jobDir(job)
		sum := Summary{
			ID:         job.ID,
			LiveStatus: job.LiveStatus,
			Status:     string(job.Status),
			CreatedAt:  job.CreatedAt,
			UpdatedAt:  job.UpdatedAt,
		}
		if job.Title != nil {
			sum.Title = *job.Title
		}
		if s := m.get(job.ID); s != nil {
			s.mu.Lock()
			sum.NextSeq, sum.Bytes, sum.LiveStatus = s.store.nextSeq, s.store.size, s.status
			s.mu.Unlock()
			sum.AutoRetranscribe = s.meta.AutoRetranscribe
		} else {
			sum.NextSeq, sum.Bytes, _ = indexSummary(dir)
			sum.AutoRetranscribe = readMeta(dir, job).AutoRetranscribe
		}
		out = append(out, sum)
	}
	return out, nil
}

// sweep is the janitor pass: it suspends open sessions that stopped receiving
// chunks, drops sessions whose job was deleted, and then starts at most one
// background finalize, only while no session is open: a finalize cut off by a
// restart, else an interrupted session that received no chunk for longer than
// AbandonAfter. Later sweeps pick up the rest.
func (m *Manager) sweep() {
	now := time.Now()

	m.mu.Lock()
	open := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		open = append(open, s)
	}
	m.mu.Unlock()

	for _, s := range open {
		s.mu.Lock()
		recording := s.status == models.LiveStatusRecording
		idle := now.Sub(s.lastChunkAt)
		s.mu.Unlock()
		if !recording {
			continue
		}
		var count int64
		if err := m.db.Model(&models.TranscriptionJob{}).Where("id = ?", s.id).Count(&count).Error; err == nil && count == 0 {
			logger.Info("Live session job was deleted, stopping its pipeline", "job_id", s.id)
			m.suspend(s, false)
			continue
		}
		if idle > m.opts.AbandonAfter {
			logger.Info("Live session stopped receiving chunks, suspending it", "job_id", s.id, "idle", idle.Round(time.Second).String())
			m.suspend(s, true)
		}
	}

	if m.resumePending() || m.anyOpen() {
		return
	}

	var jobs []models.TranscriptionJob
	if err := m.db.Select("id", "recording_path", "updated_at").
		Where("source = ? AND live_status = ?", models.SourceLive, models.LiveStatusInterrupted).
		Order("created_at asc").
		Find(&jobs).Error; err != nil {
		logger.Warn("Live janitor failed to list interrupted sessions", "error", err)
		return
	}
	for i := range jobs {
		job := &jobs[i]
		// Idle since the last chunk (the index is written for every chunk; a
		// suspend bumps updated_at), but never less than since this server started.
		last := m.startedAt
		if info, err := os.Stat(filepath.Join(m.jobDir(job), indexFileName)); err == nil {
			if info.ModTime().After(last) {
				last = info.ModTime()
			}
		} else if job.UpdatedAt.After(last) {
			last = job.UpdatedAt
		}
		if now.Sub(last) <= m.opts.AbandonAfter {
			continue
		}
		status, err := m.close(m.ctx, job.ID, nil, true)
		switch {
		case errors.Is(err, errBusy):
			return
		case err != nil:
			logger.Warn("Failed to finalize abandoned live session", "job_id", job.ID, "error", err)
		case status == models.LiveStatusFinalizing:
			logger.Info("Finalizing abandoned live session", "job_id", job.ID)
			return
		}
	}
}

// resumePending starts the next finalize that a restart cut off. It returns true
// when nothing else should start now: that finalize is running, or a session is
// open.
func (m *Manager) resumePending() bool {
	for {
		m.mu.Lock()
		if len(m.pendingFinalize) == 0 {
			m.mu.Unlock()
			return false
		}
		id := m.pendingFinalize[0]
		m.mu.Unlock()

		status, err := m.close(m.ctx, id, nil, true)
		if errors.Is(err, errBusy) {
			return true
		}
		m.mu.Lock()
		if len(m.pendingFinalize) > 0 && m.pendingFinalize[0] == id {
			m.pendingFinalize = m.pendingFinalize[1:]
		}
		m.mu.Unlock()
		switch {
		case err != nil:
			logger.Warn("Failed to resume live session finalize", "job_id", id, "error", err)
		case status == models.LiveStatusFinalizing:
			logger.Info("Resuming interrupted live session finalize", "job_id", id)
			return true
		}
	}
}

// suspend stops a recording session's pipeline and closes its files. With
// markInterrupted it stays resumable (live_status=interrupted), and only a session
// still idle for longer than AbandonAfter is suspended. The manager lock is held
// throughout so a concurrent chunk, Stop or janitor finalize cannot reopen the
// files meanwhile.
func (m *Manager) suspend(s *session, markInterrupted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s.mu.Lock()
	if s.status != models.LiveStatusRecording || (markInterrupted && time.Since(s.lastChunkAt) <= m.opts.AbandonAfter) {
		s.mu.Unlock()
		return
	}
	s.status = models.LiveStatusInterrupted
	s.mu.Unlock()

	s.cancel()
	s.waitPipeline(context.Background())
	s.saveProgress()
	s.mu.Lock()
	_ = s.store.close()
	s.mu.Unlock()
	if markInterrupted {
		if err := m.setLiveStatus(context.Background(), s.id, models.LiveStatusInterrupted); err != nil {
			logger.Warn("Failed to mark live session interrupted", "job_id", s.id, "error", err)
		}
	}
	delete(m.sessions, s.id)
}

// ErrSessionActive is returned by Remove while a session is still recording or finalizing
var ErrSessionActive = errors.New("live session is still recording or finalizing; stop it first")

// Remove deletes a live job's session directory (raw recording, index, logs) and its
// segment rows. Non-live jobs are ignored. Active sessions are refused so a meeting
// can't be deleted while it is still being captured.
func (m *Manager) Remove(ctx context.Context, job *models.TranscriptionJob) error {
	if job.Source != models.SourceLive {
		return nil
	}
	if m.get(job.ID) != nil || job.LiveStatus == models.LiveStatusRecording || job.LiveStatus == models.LiveStatusFinalizing {
		return ErrSessionActive
	}
	if err := m.db.WithContext(ctx).Where("transcription_job_id = ?", job.ID).Delete(&models.LiveSegment{}).Error; err != nil {
		logger.Warn("Failed to delete live segments", "job_id", job.ID, "error", err)
	}
	if err := os.RemoveAll(m.jobDir(job)); err != nil {
		logger.Warn("Failed to delete live session directory", "job_id", job.ID, "error", err)
	}
	return nil
}

func (m *Manager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// anyOpen reports whether any session is open (recording or finalizing)
func (m *Manager) anyOpen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions) > 0
}

func (m *Manager) removeSession(s *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[s.id] == s {
		delete(m.sessions, s.id)
	}
}

func (m *Manager) setLiveStatus(ctx context.Context, id, status string) error {
	return m.db.WithContext(ctx).Model(&models.TranscriptionJob{}).Where("id = ?", id).Update("live_status", status).Error
}

// findLiveJob loads a live job's light columns
func (m *Manager) findLiveJob(ctx context.Context, id string) (*models.TranscriptionJob, error) {
	var job models.TranscriptionJob
	err := m.db.WithContext(ctx).
		Select("id", "title", "status", "source", "live_status", "recording_path", "created_at", "updated_at").
		First(&job, "id = ?", id).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, &Error{Code: CodeNotFound}
		}
		return nil, err
	}
	if job.Source != models.SourceLive {
		return nil, &Error{Code: CodeNotFound}
	}
	return &job, nil
}

// jobDir is the session directory of a live job
func (m *Manager) jobDir(job *models.TranscriptionJob) string {
	if job.RecordingPath != nil && *job.RecordingPath != "" {
		return filepath.Dir(*job.RecordingPath)
	}
	return filepath.Join(m.opts.UploadDir, "live", job.ID)
}

// loadSession opens an existing session from disk and the database
func (m *Manager) loadSession(ctx context.Context, job *models.TranscriptionJob) (*session, error) {
	dir := m.jobDir(job)
	store, err := openChunkStore(dir)
	if err != nil {
		return nil, err
	}
	var segs []models.LiveSegment
	if err := m.db.WithContext(ctx).Where("transcription_job_id = ?", job.ID).Order("seq asc").Find(&segs).Error; err != nil {
		_ = store.close()
		return nil, fmt.Errorf("failed to load live segments: %w", err)
	}
	s := m.newSession(job.ID, dir, readMeta(dir, job), store, segs)
	s.status = job.LiveStatus
	return s, nil
}

func (m *Manager) newSession(id, dir string, meta sessionMeta, store *chunkStore, segs []models.LiveSegment) *session {
	ctx, cancel := context.WithCancel(m.ctx)
	s := &session{
		m:           m,
		id:          id,
		dir:         dir,
		meta:        meta,
		store:       store,
		lastChunkAt: time.Now(),
		g:           newGrowth(store.size),
		segments:    segs,
		activity:    time.Now(),
		ctx:         ctx,
		cancel:      cancel,
		pipeDone:    make(chan struct{}),
	}
	s.asr.engine = m.opts.Engine
	s.asr.receivedSec = meta.ReceivedAudioSec
	for _, seg := range segs {
		if seg.Seq >= s.nextSegSeq {
			s.nextSegSeq = seg.Seq + 1
		}
		if seg.EndSec > s.resumeAt {
			s.resumeAt = seg.EndSec
		}
	}
	s.asr.processedSec = s.resumeAt
	return s
}

// stateFromDisk builds the state of a session that is not open
func (m *Manager) stateFromDisk(ctx context.Context, job *models.TranscriptionJob, after int) (*State, error) {
	dir := m.jobDir(job)
	next, size, err := indexSummary(dir)
	if err != nil {
		return nil, err
	}
	var segs []models.LiveSegment
	if err := m.db.WithContext(ctx).Where("transcription_job_id = ? AND seq > ?", job.ID, after).Order("seq asc").Find(&segs).Error; err != nil {
		return nil, fmt.Errorf("failed to load live segments: %w", err)
	}
	var processed float64
	var last models.LiveSegment
	if err := m.db.WithContext(ctx).Where("transcription_job_id = ?", job.ID).Order("seq desc").Limit(1).Find(&last).Error; err == nil {
		processed = last.EndSec
	}
	meta := readMeta(dir, job)
	received := meta.ReceivedAudioSec
	if processed > received {
		received = processed
	}

	st := &State{
		ID:               job.ID,
		LiveStatus:       job.LiveStatus,
		Status:           string(job.Status),
		NextSeq:          next,
		Bytes:            size,
		ReceivedAudioSec: round3(received),
		ASR:              ASRState{State: asrStopped, ProcessedSec: processed, Engine: m.opts.Engine},
		Segments:         toSegments(segs),
		AutoRetranscribe: meta.AutoRetranscribe,
	}
	if job.Title != nil {
		st.Title = *job.Title
	}
	return st, nil
}

// snapshot builds the state of an open session
func (s *session) snapshot(job *models.TranscriptionJob, after int) *State {
	s.mu.Lock()
	next, size, status := s.store.nextSeq, s.store.size, s.status
	s.mu.Unlock()

	s.asrMu.Lock()
	asr := s.asr
	var partial *Partial
	if asr.partial != nil {
		p := *asr.partial
		partial = &p
	}
	i := sort.Search(len(s.segments), func(i int) bool { return s.segments[i].Seq > after })
	segs := toSegments(s.segments[i:])
	s.asrMu.Unlock()

	state, errMsg := asr.state, asr.errMsg
	if state == "" {
		// Decoding without a worker: the environment is still being prepared or failed.
		envState, envErr := s.m.env.status()
		if envState == envFailed {
			state, errMsg = asrUnavailable, envErr
		} else {
			state = asrStarting
		}
	}
	lag := asr.receivedSec - asr.processedSec
	if lag < 0 || state != asrRunning {
		lag = 0
	}

	st := &State{
		ID:               s.id,
		Title:            s.meta.Title,
		LiveStatus:       status,
		Status:           string(job.Status),
		NextSeq:          next,
		Bytes:            size,
		ReceivedAudioSec: round3(asr.receivedSec),
		ASR: ASRState{
			State:        state,
			ProcessedSec: round3(asr.processedSec),
			LagSec:       round3(lag),
			Engine:       asr.engine,
			Restarts:     asr.restarts,
			Error:        errMsg,
		},
		Segments:         segs,
		Partial:          partial,
		AutoRetranscribe: s.meta.AutoRetranscribe,
	}
	if job.Title != nil {
		st.Title = *job.Title
	}
	return st
}

func toSegments(segs []models.LiveSegment) []Segment {
	out := make([]Segment, 0, len(segs))
	for _, seg := range segs {
		out = append(out, Segment{Seq: seg.Seq, Start: seg.StartSec, End: seg.EndSec, Text: seg.Text})
	}
	return out
}

// startPipeline starts the supervisor once
func (s *session) startPipeline() {
	s.pipeOnce.Do(func() {
		s.asrMu.Lock()
		s.pipeStarted = true
		s.asrMu.Unlock()
		go s.supervise()
	})
}

// waitPipeline waits for a started pipeline to exit
func (s *session) waitPipeline(ctx context.Context) {
	s.asrMu.Lock()
	started := s.pipeStarted
	s.asrMu.Unlock()
	if !started {
		return
	}
	select {
	case <-s.pipeDone:
	case <-ctx.Done():
	}
}

func (s *session) touchLocked() {
	s.activity = time.Now()
}

func (s *session) lastActivity() time.Time {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	return s.activity
}

func (s *session) setReceived(sec float64) {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	if sec > s.asr.receivedSec {
		s.asr.receivedSec = sec
	}
	s.touchLocked()
}

// backlog is the decoded audio the worker has not processed yet
func (s *session) backlog() time.Duration {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	sec := s.asr.receivedSec - s.asr.processedSec
	if sec < 0 {
		sec = 0
	}
	return time.Duration(sec * float64(time.Second))
}

func (s *session) setASRState(state, errMsg string) {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	s.asr.state = state
	s.asr.errMsg = errMsg
	s.asr.partial = nil
}

// saveProgress records the decoded audio length (and the client's last seq) in
// session.json, so the state of a closed session still reports it
func (s *session) saveProgress() {
	s.asrMu.Lock()
	received := s.asr.receivedSec
	s.asrMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.meta
	meta.ReceivedAudioSec = round3(received)
	if err := writeMeta(s.dir, meta); err != nil {
		logger.Warn("Failed to save live session progress", "job_id", s.id, "error", err)
	}
}

// setLastSeqLocked records the highest seq the client recorded, when it sent one.
// s.mu must be held (or s not yet shared).
func (s *session) setLastSeqLocked(lastSeq *int) {
	if lastSeq != nil {
		v := *lastSeq
		s.meta.LastSeq = &v
	}
}

func (s *session) segmentsSnapshot() []models.LiveSegment {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	return append([]models.LiveSegment(nil), s.segments...)
}

func (s *session) engine() string {
	s.asrMu.Lock()
	defer s.asrMu.Unlock()
	return s.asr.engine
}

// writeMeta atomically and durably writes session.json
func writeMeta(dir string, meta sessionMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, metaFileName)
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return fmt.Errorf("failed to write session metadata: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed to write session metadata: %w", err)
	}
	return nil
}

// readMeta reads session.json, falling back to the job's title
func readMeta(dir string, job *models.TranscriptionJob) sessionMeta {
	var meta sessionMeta
	if data, err := os.ReadFile(filepath.Join(dir, metaFileName)); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	if meta.Title == "" && job.Title != nil {
		meta.Title = *job.Title
	}
	return meta
}

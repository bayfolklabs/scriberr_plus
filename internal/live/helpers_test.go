package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scriberr/internal/models"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// The test binary doubles as the fake decoder and the fake live worker, so the
// pipeline tests do not depend on Python (or, for most tests, on ffmpeg).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "fake-live-worker":
			os.Exit(fakeWorker())
		case "fake-live-worker-hang":
			// Writes its PID, reports ready and then never exits, even on stdin EOF.
			_ = os.WriteFile(os.Args[2], []byte(fmt.Sprint(os.Getpid())), 0644)
			fmt.Println(`{"type":"ready","engine":"hang","sample_rate":16000}`)
			_, _ = io.Copy(io.Discard, os.Stdin)
			time.Sleep(time.Hour)
			os.Exit(0)
		case "fake-decoder":
			// Treat the "recording" as raw PCM: copy stdin to stdout.
			_, _ = io.Copy(os.Stdout, os.Stdin)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

const (
	fakeSegmentSamples = 2 * sampleRate // the fake worker emits one final per 2 s of audio
	crashMarkerEnv     = "FAKE_LIVE_CRASH_MARKER"
)

// fakeWorker speaks the live worker protocol: PCM on stdin, JSON lines on stdout.
// If FAKE_LIVE_CRASH_MARKER names a missing file, it creates the file and exits 3
// after 3 s of audio (once), to exercise the supervisor's restart path.
func fakeWorker() int {
	out := bufio.NewWriter(os.Stdout)
	emit := func(v map[string]interface{}) {
		data, _ := json.Marshal(v)
		out.Write(data)
		out.WriteByte('\n')
		out.Flush()
	}

	emit(map[string]interface{}{"type": "ready", "engine": "fake", "sample_rate": sampleRate})

	var totalBytes, segStart, lastProgress int64
	n := 0
	final := func(from, to int64) {
		start, end := float64(from)/sampleRate, float64(to)/sampleRate
		ev := map[string]interface{}{
			"type":  "final",
			"text":  fmt.Sprintf("fake segment %d text", n),
			"start": start,
			"end":   end,
		}
		if n%2 == 0 { // even segments carry word timings, odd ones exercise the fallback
			step := (end - start) / 4
			words := []map[string]interface{}{}
			for i, w := range []string{"fake", "segment", fmt.Sprint(n), "text"} {
				words = append(words, map[string]interface{}{"word": w, "start": start + float64(i)*step, "end": start + float64(i+1)*step})
			}
			ev["words"] = words
		}
		emit(ev)
		n++
	}

	marker := os.Getenv(crashMarkerEnv)
	buf := make([]byte, 8192)
	for {
		k, err := os.Stdin.Read(buf)
		totalBytes += int64(k)
		total := totalBytes / bytesPerSample
		for total-segStart >= fakeSegmentSamples {
			final(segStart, segStart+fakeSegmentSamples)
			segStart += fakeSegmentSamples
		}
		if total-lastProgress >= sampleRate {
			lastProgress = total
			emit(map[string]interface{}{"type": "progress", "processed_sec": float64(total) / sampleRate})
			if total > segStart {
				emit(map[string]interface{}{"type": "partial", "text": "fake partial", "start": float64(segStart) / sampleRate})
			}
		}
		if marker != "" && total >= 3*sampleRate {
			if _, statErr := os.Stat(marker); os.IsNotExist(statErr) {
				_ = os.WriteFile(marker, []byte("crashed"), 0644)
				fmt.Fprintln(os.Stderr, "fake worker: simulated crash")
				return 3
			}
		}
		if err != nil {
			break
		}
	}
	if total := totalBytes / bytesPerSample; total > segStart {
		final(segStart, total)
	}
	emit(map[string]interface{}{"type": "eof"})
	return 0
}

// newTestDB opens a throwaway SQLite database with the live tables
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "live_test.db")
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.TranscriptionJob{}, &models.LiveSegment{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return db
}

// testExe returns the test binary path, used as fake decoder and worker
func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	if strings.ContainsAny(exe, " \t") {
		t.Skip("test binary path contains spaces; LIVE_WORKER_CMD is split on whitespace")
	}
	return exe
}

// newTestManager returns a started manager that uses the fake worker. With
// fakeDecoder the recording is treated as raw PCM instead of WebM.
func newTestManager(t *testing.T, db *gorm.DB, uploadDir string, fakeDecoder bool) *Manager {
	t.Helper()
	exe := testExe(t)
	m := NewManager(db, Options{
		UploadDir:    uploadDir,
		EnvDir:       filepath.Join(uploadDir, "env"),
		ModelsDir:    filepath.Join(uploadDir, "models"),
		Engine:       "fake",
		AbandonAfter: time.Hour,
		WorkerCmd:    exe + " fake-live-worker",
	})
	if fakeDecoder {
		m.decoderCmd = []string{exe, "fake-decoder"}
	}
	m.drainTimeout = 20 * time.Second
	m.janitorInterval = time.Hour // tests call sweep directly
	m.Start()
	t.Cleanup(func() { shutdownManager(t, m) })
	return m
}

func shutdownManager(t *testing.T, m *Manager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m.Shutdown(ctx)
}

// pcm returns sec seconds of a quiet 440 Hz tone as s16le mono 16 kHz
func pcm(sec float64) []byte {
	n := int(sec * sampleRate)
	out := make([]byte, n*bytesPerSample)
	for i := 0; i < n; i++ {
		v := int16(1000)
		if (i/18)%2 == 0 {
			v = -1000
		}
		out[2*i] = byte(v)
		out[2*i+1] = byte(uint16(v) >> 8)
	}
	return out
}

// split cuts data into chunks of at most size bytes
func split(data []byte, size int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := size
		if n > len(data) {
			n = len(data)
		}
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

// waitFor polls cond until it returns true or the timeout expires
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// loadJob reads the full job row
func loadJob(t *testing.T, db *gorm.DB, id string) models.TranscriptionJob {
	t.Helper()
	var job models.TranscriptionJob
	if err := db.First(&job, "id = ?", id).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	return job
}

// waitFinalized waits until the job leaves the finalizing state
func waitFinalized(t *testing.T, db *gorm.DB, id string) models.TranscriptionJob {
	t.Helper()
	var job models.TranscriptionJob
	waitFor(t, 60*time.Second, "finalize", func() bool {
		job = loadJob(t, db, id)
		return job.LiveStatus == models.LiveStatusFinalized || job.LiveStatus == models.LiveStatusFailed
	})
	if job.LiveStatus != models.LiveStatusFinalized {
		msg := ""
		if job.ErrorMessage != nil {
			msg = *job.ErrorMessage
		}
		t.Fatalf("live_status = %s (%s), want finalized", job.LiveStatus, msg)
	}
	return job
}

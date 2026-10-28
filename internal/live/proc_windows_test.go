//go:build windows
// +build windows

package live

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"scriberr/internal/models"
)

// processAlive reports whether a PID is running, using tasklist
func processAlive(t *testing.T, pid int) bool {
	t.Helper()
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		t.Fatalf("tasklist: %v", err)
	}
	return strings.Contains(string(out), `"`+strconv.Itoa(pid)+`"`)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, 20*time.Second, "worker pid file", func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(data)))
		return err == nil && pid > 0
	})
	return pid
}

// TestProcessTreeKilled runs the worker as a grandchild (cmd.exe -> worker) that
// ignores stdin EOF, and checks that neither a stuck finalize nor a server
// shutdown leaves it running.
func TestProcessTreeKilled(t *testing.T) {
	exe := testExe(t)
	db := newTestDB(t)
	uploads := t.TempDir()
	pidDir := t.TempDir()
	ctx := context.Background()

	newManager := func(pidFile string) *Manager {
		m := NewManager(db, Options{
			UploadDir: uploads,
			EnvDir:    filepath.Join(uploads, "env"),
			ModelsDir: filepath.Join(uploads, "models"),
			WorkerCmd: "cmd /c " + quoteArg(exe) + " fake-live-worker-hang " + quoteArg(pidFile),
		})
		m.decoderCmd = []string{exe, "fake-decoder"}
		m.drainTimeout = 2 * time.Second
		m.janitorInterval = time.Hour
		m.Start()
		return m
	}

	// Finalize: the worker never flushes, so finalize kills the tree after drainTimeout.
	pidA := filepath.Join(pidDir, "a.pid")
	m := newManager(pidA)
	job, err := m.CreateSession(ctx, CreateRequest{Title: "stuck"})
	if err != nil {
		t.Fatal(err)
	}
	feed(t, m, job.ID, 0, split(pcm(1), sampleRate))
	pid := readPID(t, pidA)
	if !processAlive(t, pid) {
		t.Fatal("worker grandchild is not running")
	}
	if _, err := m.Finalize(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	waitFinalized(t, db, job.ID)
	waitFor(t, 10*time.Second, "worker killed after finalize", func() bool { return !processAlive(t, pid) })

	// Shutdown: the tree is killed and the session stays resumable.
	pidB := filepath.Join(pidDir, "b.pid")
	m2 := newManager(pidB)
	job2, err := m2.CreateSession(ctx, CreateRequest{Title: "shutdown"})
	if err != nil {
		t.Fatal(err)
	}
	feed(t, m2, job2.ID, 0, split(pcm(1), sampleRate))
	pid = readPID(t, pidB)
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m.Shutdown(shutdownCtx)
	m2.Shutdown(shutdownCtx)
	waitFor(t, 10*time.Second, "worker killed on shutdown", func() bool { return !processAlive(t, pid) })
	if got := loadJob(t, db, job2.ID).LiveStatus; got != models.LiveStatusRecording {
		t.Fatalf("live_status after shutdown = %s, want recording", got)
	}
}

package live

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"scriberr/pkg/logger"
)

// Live Python environment states
const (
	envIdle      = "idle"
	envPreparing = "preparing"
	envReady     = "ready"
	envFailed    = "failed"
)

const (
	envImportCheck   = "import sherpa_onnx"
	envSetupTimeout  = 30 * time.Minute
	envRetryInterval = 5 * time.Minute
)

// pythonEnv prepares <WHISPERX_ENV>/live in the background and tracks readiness.
// Recording never waits for it: until it is ready the pipeline only decodes audio.
type pythonEnv struct {
	dir       string // absolute
	script    []byte
	pyproject []byte

	mu          sync.Mutex
	state       string
	errMsg      string
	ready       chan struct{} // closed once the environment is ready
	lastAttempt time.Time
}

func newPythonEnv(dir string, script, pyproject []byte, external bool) *pythonEnv {
	e := &pythonEnv{dir: dir, script: script, pyproject: pyproject, state: envIdle, ready: make(chan struct{})}
	if external {
		// LIVE_WORKER_CMD replaces the uv worker; there is nothing to prepare.
		e.state = envReady
		close(e.ready)
	}
	return e
}

// status returns the current state and the last preparation error
func (e *pythonEnv) status() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, e.errMsg
}

// readyCh is closed once the environment is ready
func (e *pythonEnv) readyCh() <-chan struct{} {
	return e.ready
}

func (e *pythonEnv) isReady() bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// ensure starts preparation in the background unless it is ready, already
// running, or failed recently. The goroutine is tracked by wg.
func (e *pythonEnv) ensure(ctx context.Context, wg *sync.WaitGroup) {
	e.mu.Lock()
	switch {
	case e.state == envReady || e.state == envPreparing:
		e.mu.Unlock()
		return
	case e.state == envFailed && time.Since(e.lastAttempt) < envRetryInterval:
		e.mu.Unlock()
		return
	}
	e.state = envPreparing
	e.lastAttempt = time.Now()
	e.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		start := time.Now()
		err := e.prepare(ctx)

		e.mu.Lock()
		defer e.mu.Unlock()
		if err != nil {
			e.state = envFailed
			e.errMsg = err.Error()
			if ctx.Err() == nil {
				logger.Warn("Live ASR environment is not available; live sessions will record without transcription",
					"env_path", e.dir, "error", err)
			}
			return
		}
		e.state = envReady
		e.errMsg = ""
		close(e.ready)
		logger.Info("Live ASR environment ready", "env_path", e.dir, "duration", time.Since(start).Round(time.Second).String())
	}()
}

// prepare writes the embedded worker files and runs `uv sync` when sherpa_onnx
// cannot be imported yet.
func (e *pythonEnv) prepare(ctx context.Context) error {
	logger.Info("Preparing live ASR environment", "env_path", e.dir)
	if err := os.MkdirAll(e.dir, 0755); err != nil {
		return fmt.Errorf("failed to create live environment directory: %w", err)
	}

	// Only rewrite pyproject.toml when it changed, so uv sees no spurious edits.
	pyprojectPath := filepath.Join(e.dir, "pyproject.toml")
	if current, err := os.ReadFile(pyprojectPath); err != nil || !bytes.Equal(current, e.pyproject) {
		if err := os.WriteFile(pyprojectPath, e.pyproject, 0644); err != nil {
			return fmt.Errorf("failed to write pyproject.toml: %w", err)
		}
	}
	// Always overwrite the worker so script updates apply on restart.
	if err := os.WriteFile(filepath.Join(e.dir, "live_worker.py"), e.script, 0755); err != nil {
		return fmt.Errorf("failed to write live_worker.py: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, envSetupTimeout)
	defer cancel()

	if e.importCheck(ctx) == nil {
		return nil
	}

	logger.Info("Installing live ASR dependencies", "env_path", e.dir)
	cmd := exec.CommandContext(ctx, "uv", "sync", "--native-tls")
	cmd.Dir = e.dir
	prepareEnvCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("uv sync failed: %w: %s", err, lastLines(out, 5))
	}

	if err := e.importCheck(ctx); err != nil {
		return fmt.Errorf("sherpa_onnx import failed after uv sync: %w", err)
	}
	return nil
}

// importCheck runs `uv run --project <dir> python -c "import sherpa_onnx"`. Unlike
// adapters.CheckEnvironmentReady it is not cached, so it can be re-run after a sync.
func (e *pythonEnv) importCheck(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "uv", "run", "--native-tls", "--project", e.dir, "python", "-c", envImportCheck)
	prepareEnvCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, lastLines(out, 3))
	}
	return nil
}

// lastLines returns the last n non-empty lines of command output
func lastLines(out []byte, n int) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

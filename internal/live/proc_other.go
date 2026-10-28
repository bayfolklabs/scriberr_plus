//go:build !windows
// +build !windows

package live

import (
	"os/exec"
	"syscall"
	"time"
)

// prepareCmd puts the child in its own process group so cancelling its context
// kills the whole tree (`uv run` spawns python as a child).
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	prepareEnvCmd(cmd)
}

// prepareEnvCmd makes cancelling the environment setup commands (uv sync, the
// import check) kill them. They stay in the server's process group, so a terminal
// Ctrl+C during startup reaches them like the batch uv children.
func prepareEnvCmd(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		killProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// killProcessTree sends SIGKILL to the child's process group, or to the child
// alone when it has no group of its own.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

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
	cmd.Cancel = func() error {
		killProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// killProcessTree sends SIGKILL to the child's process group.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

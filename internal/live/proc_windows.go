//go:build windows
// +build windows

package live

import (
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// prepareCmd makes cancelling a child's context kill its whole process tree. The
// worker runs as `uv run ... python`, so killing only uv.exe would orphan python.exe.
// CREATE_NEW_PROCESS_GROUP keeps a console Ctrl+C aimed at the server from reaching
// the children; the server's own shutdown stops them instead.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error {
		killProcessTree(cmd)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// killProcessTree kills the process and all of its descendants.
func killProcessTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

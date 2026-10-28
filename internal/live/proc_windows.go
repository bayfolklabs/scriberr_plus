//go:build windows
// +build windows

package live

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// taskkillTimeout bounds one taskkill run, so a stuck taskkill cannot hold up a
// teardown (suspend holds the manager lock while it kills a pipeline)
const taskkillTimeout = 10 * time.Second

// prepareCmd makes cancelling a child's context kill its whole process tree. The
// worker runs as `uv run ... python`, so killing only uv.exe would orphan python.exe.
// CREATE_NEW_PROCESS_GROUP keeps a console Ctrl+C aimed at the server from reaching
// the children; the server's own shutdown stops them instead.
func prepareCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	prepareEnvCmd(cmd)
}

// prepareEnvCmd makes cancelling the environment setup commands (uv sync, the
// import check) kill their process tree. They stay in the server's console process
// group, so a Ctrl+C during startup, before the server handles signals, reaches
// them like the batch uv children.
func prepareEnvCmd(cmd *exec.Cmd) {
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
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := kill.Run(); err != nil {
		_ = cmd.Process.Kill()
	}
}

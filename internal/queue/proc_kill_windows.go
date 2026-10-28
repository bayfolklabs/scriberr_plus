//go:build windows
// +build windows

package queue

import (
	"os"
	"os/exec"
	"strconv"
)

// killProcessTree kills the process and all of its descendants. Model jobs run
// as `uv run ... python`, so killing only uv.exe would orphan the python child.
func killProcessTree(p *os.Process) error {
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run(); err != nil {
		return p.Kill()
	}
	return nil
}

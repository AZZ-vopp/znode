//go:build !windows

package terminal

import (
	"os/exec"
	"syscall"
)

func terminatePTYProcess(cmd *exec.Cmd, force bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	// pty.Start creates a session whose process group matches the shell PID.
	// Signal the group so child processes do not survive a closed relay.
	_ = syscall.Kill(-cmd.Process.Pid, signal)
}

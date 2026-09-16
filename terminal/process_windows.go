//go:build windows

package terminal

import "os/exec"

func terminatePTYProcess(cmd *exec.Cmd, _ bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Windows does not expose Unix process-group signals through syscall.
	// Killing the PTY shell keeps cross-platform release builds deterministic.
	_ = cmd.Process.Kill()
}

//go:build unix

package builtin

import (
	"os/exec"
	"syscall"
)

// setGroup starts the command in a new process group, so a kill reaches its
// children too.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// killGroup kills the process group of the command.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

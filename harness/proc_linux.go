//go:build linux

package harness

import (
	"os/exec"
	"syscall"
)

// procAttr puts a process of the harness in a process group of its own,
// so a stop reaches the processes that it starts, and ends it with fw: a
// harness never outlives the process that hosts it.
func procAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}

// stop sends sig to the process group of cmd.
func stop(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

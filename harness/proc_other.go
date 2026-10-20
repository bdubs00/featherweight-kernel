//go:build !linux

package harness

import (
	"os/exec"
	"syscall"
)

// procAttr leaves cmd as it is on a system other than Linux.
func procAttr(*exec.Cmd) {}

// stop ends the process of cmd.
func stop(cmd *exec.Cmd, _ syscall.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

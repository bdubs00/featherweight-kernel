//go:build !unix

package builtin

import "os/exec"

// setGroup does nothing on this platform.
func setGroup(*exec.Cmd) {}

// killGroup kills the command. Its children keep running on this platform.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

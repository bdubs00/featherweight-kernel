//go:build !unix

package fw

import (
	"errors"
	"os/exec"
)

const toolUserSupported = false

// Apply does nothing on this platform, where LookupToolUser gives no user.
func (u *ToolUser) Apply(*exec.Cmd) {}

// useToolUser is never reached on this platform.
func (a *Agent) useToolUser(*ToolUser) error {
	return errors.New("fw: tools.user needs Linux or another Unix system")
}

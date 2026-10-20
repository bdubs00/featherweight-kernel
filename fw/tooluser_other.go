//go:build !unix

package fw

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
)

const toolUserSupported = false

// Apply does nothing on this platform, where LookupToolUser gives no user.
func (u *ToolUser) Apply(*exec.Cmd) {}

// useToolUser is never reached on this platform.
func (a *Agent) useToolUser(*ToolUser) error {
	return errors.New("fw: tools.user needs Linux or another Unix system")
}

// ownSub is never reached on this platform.
func ownSub(*os.Root, string, fs.FileInfo, *ToolUser, fs.FileMode, bool) error {
	return errors.New("fw: tools.user needs Linux or another Unix system")
}

// guardMode is the mode of a directory that holds the state of fw.
const guardMode = fs.ModeDir | fs.ModeSticky | 0o775

//go:build !unix

package env

import (
	"io/fs"
	"os/exec"
)

// tryLock always succeeds on this platform, which has no flock(2): two
// processes on one environment can then install it at the same time.
func tryLock(string) (func(), bool, error) { return func() {}, true, nil }

// locked always reports false on this platform.
func locked(string) bool { return false }

// ownedBySelf trusts every file on this platform, which has no tool user.
func ownedBySelf(fs.FileInfo) bool { return true }

// sameDevice assumes one file system on this platform.
func sameDevice(string, string) bool { return true }

// own does nothing on this platform, which has no tool user.
func (s *spec) own(string) error { return nil }

// guard does nothing on this platform.
func (s *spec) guard(string) error { return nil }

// processGroup does nothing on this platform.
func processGroup(*exec.Cmd) {}

//go:build unix

package env

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// tryLock takes the lock file at path without waiting. The lock is a
// flock(2) lock, so it holds against another process and against another
// descriptor of this process, and it ends with the process. It reports
// false when another holder has it.
func tryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

// locked reports whether another holder has the lock file at path. It
// never creates the file.
func locked(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// ownedBySelf reports whether the user of this process owns a file.
func ownedBySelf(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}

// sameDevice reports whether two paths are on one file system. A path that
// does not exist yet counts with its nearest parent that does.
func sameDevice(a, b string) bool {
	da, oka := device(a)
	db, okb := device(b)
	return oka && okb && da == db
}

func device(p string) (uint64, bool) {
	for {
		var st syscall.Stat_t
		if err := syscall.Stat(p, &st); err == nil {
			return uint64(st.Dev), true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, false
		}
		p = parent
	}
}

// own gives a path to the tool user, when fw runs as root and a tool user
// exists. Without CAP_CHOWN the call fails, but then fw.LookupToolUser
// gives no tool user (section 7.12 of the architecture).
func (s *spec) own(p string) error {
	if s.user == nil || os.Geteuid() != 0 {
		return nil
	}
	return os.Lchown(p, int(s.user.UID), int(s.user.GID))
}

// guard gives dir to the user of fw with the group of the tool user and
// the mode 1775: the tool user adds and removes its own entries, such as
// the virtual environment, but never renames or removes a state file of
// fw (the sticky bit).
func (s *spec) guard(dir string) error {
	if s.user == nil || os.Geteuid() != 0 {
		return nil
	}
	if err := os.Lchown(dir, os.Geteuid(), int(s.user.GID)); err != nil {
		return err
	}
	return os.Chmod(dir, os.ModeSticky|0o775)
}

// processGroup starts cmd in a process group of its own and kills the
// whole group when its context ends, so a build that a manager starts
// ends with it.
func processGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

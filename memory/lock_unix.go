//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package memory

import (
	"fmt"
	"os"
	"syscall"
)

// lockDir takes the exclusive lock of a scope directory and returns its
// release. It locks the inode of the directory, so every Store of the
// process and every process on the volume contend on it, and the system
// releases it when the process ends.
func lockDir(dir string) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// inodeOf returns the inode of a file, or 0 when the system gives none. An
// atomic write renames a new file over a note, so a new inode marks a
// change that the time and the size of the note can miss.
func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockHeld is true: this system orders the writers with flock.
const lockHeld = true

// lockDir takes the exclusive lock of a scope directory and returns its
// release. It locks the inode of the directory, so every Store of the
// process and every process on the volume contend on it, and the system
// releases it when the process ends. It tries the lock without blocking,
// and again after a short wait that grows to lockPollMax, until it has the
// lock, ctx ends, or lockWait passes. Then it returns an error that wraps
// the error of ctx or ErrLockTimeout.
func lockDir(ctx context.Context, dir string) (func(), error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
	}
	deadline := time.Now().Add(lockWait)
	wait := time.Millisecond
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
		}
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
		}
		left := time.Until(deadline)
		if left <= 0 {
			_ = f.Close()
			return nil, fmt.Errorf("memory: lock %s: another writer held it for %s: %w", dir, lockWait, ErrLockTimeout)
		}
		t := time.NewTimer(min(wait, left))
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
		wait = min(wait*2, lockPollMax)
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

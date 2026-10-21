//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package fw

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock the directory inode, not a removable lock file. Independent opens,
// including in one process and through symlink aliases, contend on this inode.
// The descriptor is close-on-exec and the OS releases its lock on process exit.
func lockLedgerDirectory(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("fw: file ledger: open ownership directory: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrLedgerLocked, path)
		}
		return nil, fmt.Errorf("fw: file ledger: lock writer ownership of %s: %w", path, err)
	}
	return f, nil
}

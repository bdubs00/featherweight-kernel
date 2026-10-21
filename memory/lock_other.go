//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package memory

import (
	"context"
	"fmt"
	"os"
)

// lockHeld is false: this system has no flock, so no lock orders the
// writers.
const lockHeld = false

// lockDir takes no lock on a system without flock. A compare-and-swap and
// the count of notes are then checked without one, so two writers at the
// same moment can both pass. It still refuses a context that ended.
func lockDir(ctx context.Context, dir string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("memory: lock %s: %w", dir, err)
	}
	return func() {}, nil
}

// inodeOf returns 0: this system gives no inode.
func inodeOf(os.FileInfo) uint64 { return 0 }

//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package memory

import "os"

// lockDir takes no lock on a system without flock. A compare-and-swap and
// the count of notes are then checked without one, so two writers at the
// same moment can both pass.
func lockDir(string) (func(), error) { return func() {}, nil }

// inodeOf returns 0: this system gives no inode.
func inodeOf(os.FileInfo) uint64 { return 0 }

//go:build !linux

package cli

// undumpable does nothing on this platform, which has no PR_SET_DUMPABLE.
func undumpable() error { return nil }

// dumpable reports true on this platform.
func dumpable() (bool, error) { return true, nil }

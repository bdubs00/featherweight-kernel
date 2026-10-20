//go:build linux

package cli

import "syscall"

// undumpable makes this process non-dumpable (prctl PR_SET_DUMPABLE 0).
// A process of another user, and a process of the same user without
// CAP_SYS_PTRACE, then cannot attach to fw or read its memory, its
// environment, or its other entries under /proc, and fw leaves no core
// file. A tool process that fw starts is dumpable again after its exec,
// and it reads none of the entries of fw (docs/ARCHITECTURE.md section
// 7.12).
func undumpable() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// dumpable reports the flag of this process, for tests.
func dumpable() (bool, error) {
	r, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if errno != 0 {
		return false, errno
	}
	return r != 0, nil
}

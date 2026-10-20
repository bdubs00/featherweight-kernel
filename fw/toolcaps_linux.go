//go:build linux

package fw

import "os"

// MissingToolCaps returns the capabilities that tools.user needs and that
// the effective set of this process lacks, such as ["CAP_CHOWN",
// "CAP_SETUID"], or nil when it has them all. Only the answer for root
// matters: a process of another user never starts a tool as a third one.
// It returns nil when /proc/self/status cannot tell, and the start then
// stops at the first change of owner with the error of the system.
func MissingToolCaps() []string {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return nil
	}
	return missingToolCaps(string(b))
}

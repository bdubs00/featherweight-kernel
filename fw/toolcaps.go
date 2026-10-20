package fw

import (
	"strconv"
	"strings"
)

// toolCaps are the capabilities (capabilities(7)) that root needs for
// tools.user. A sandbox can take them from root: gVisor under Agent
// Substrate leaves root CAP_KILL, CAP_NET_BIND_SERVICE, and CAP_AUDIT_WRITE
// only, so a chown and a start as another user both fail there.
var toolCaps = []struct {
	bit  uint
	name string
}{
	{0, "CAP_CHOWN"},        // gives the workspace and the new files to the tool user
	{1, "CAP_DAC_OVERRIDE"}, // reads and writes the files of the tool user
	{5, "CAP_KILL"},         // stops the processes of a tool at its time limit
	{6, "CAP_SETGID"},       // starts a process of a tool with the group of the tool user
	{7, "CAP_SETUID"},       // starts a process of a tool as the tool user
}

// missingToolCaps returns the names of toolCaps that the CapEff line of a
// /proc/<pid>/status file lacks, or nil when the text has no such line.
func missingToolCaps(status string) []string {
	for line := range strings.Lines(status) {
		v, ok := strings.CutPrefix(line, "CapEff:")
		if !ok {
			continue
		}
		eff, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		if err != nil {
			return nil
		}
		var missing []string
		for _, c := range toolCaps {
			if eff&(1<<c.bit) == 0 {
				missing = append(missing, c.name)
			}
		}
		return missing
	}
	return nil
}

package fw

import (
	"slices"
	"testing"
)

// The status line of /proc names the capabilities that tools.user lacks.
func TestMissingToolCaps(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         []string
	}{
		{"the default set of Docker", "Name:\tfw\nCapInh:\t0000000000000000\nCapEff:\t00000000a80425fb\n", nil},
		{"every capability", "CapEff:\t000001ffffffffff\n", nil},
		{"root in gVisor under Agent Substrate", "CapPrm:\t0000000020000420\nCapEff:\t0000000020000420\n", []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_SETGID", "CAP_SETUID"}},
		{"no capability", "CapEff:\t0000000000000000", []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_KILL", "CAP_SETGID", "CAP_SETUID"}},
		{"no CapEff line", "Name:\tfw\n", nil},
		{"a value that is not a number", "CapEff:\tzz\n", nil},
	} {
		if got := missingToolCaps(tc.status); !slices.Equal(got, tc.want) {
			t.Errorf("%s: missingToolCaps = %v, want %v", tc.name, got, tc.want)
		}
	}
}

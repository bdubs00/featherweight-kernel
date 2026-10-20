package ax

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// TestChildBudgetCarriesAReportThatIsOff sends finalReport to a child in
// another process only when it is off, so a child never reports where its
// parent makes none, and a child of an older kernel sees no new field.
func TestChildBudgetCarriesAReportThatIsOff(t *testing.T) {
	on, off := true, false
	for _, c := range []struct {
		report *bool
		want   bool
	}{{nil, false}, {&on, false}, {&off, true}} {
		b := fw.Budget{MaxTurns: 3, Deadline: time.Minute, FinalReport: c.report, ReportMaxTokens: 1024, ReportTimeout: 2 * time.Minute}
		raw, err := json.Marshal(budgetOf(&b))
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		if got := strings.Contains(s, `"finalReport":false`); got != c.want || strings.Contains(s, "reportMaxTokens") || strings.Contains(s, "reportTimeout") {
			t.Errorf("report %v: body %s", c.report, s)
		}
	}
}

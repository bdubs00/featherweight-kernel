package observability_test

import (
	"encoding/json"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDashboardAndAlertsParse(t *testing.T) {
	b, err := os.ReadFile("grafana/fw.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			ID      int
			Targets []struct{ Expr string }
		}
	}
	if err := json.Unmarshal(b, &dashboard); err != nil {
		t.Fatal(err)
	}
	ids := map[int]bool{}
	for _, p := range dashboard.Panels {
		if ids[p.ID] {
			t.Fatalf("duplicate panel id %d", p.ID)
		}
		ids[p.ID] = true
	}
	b, err = os.ReadFile("alerts.yml")
	if err != nil {
		t.Fatal(err)
	}
	var alerts struct {
		Groups []struct {
			Name  string
			Rules []struct {
				Alert string
				Expr  string
			}
		}
	}
	if err := yaml.Unmarshal(b, &alerts); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, g := range alerts.Groups {
		for _, r := range g.Rules {
			if r.Alert == "" || r.Expr == "" || names[r.Alert] {
				t.Fatalf("invalid/duplicate alert in %s: %+v", g.Name, r)
			}
			names[r.Alert] = true
		}
	}
	if !names["FwStorageBlocked"] {
		t.Fatal("missing alert FwStorageBlocked")
	}
	for _, name := range []string{"FwVerifiedCompletionRateLow", "FwOperatorInterventionsHigh", "FwRecoverySlow", "FwConfirmedDuplicateEffects", "FwSuccessfulTaskCostUnknown", "FwSuccessfulTaskCostHigh"} {
		if !names[name] {
			t.Fatalf("missing quality alert %s", name)
		}
	}
}

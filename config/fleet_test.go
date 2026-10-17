package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestValidateFleet(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Fleet = fw.FleetConfig{
		Subject:  "fw..fleet",
		Name:     "two words",
		URL:      "ax:team-a",
		Forward:  []string{"*.agents.svc"},
		Interval: -time.Second,
		Expire:   -time.Hour,
	}
	err := Validate(cfg)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Validate = %v", err)
	}
	for _, want := range []string{
		`fleet.subject "fw..fleet" is not a subject`,
		`fleet.name "two words" holds a space`,
		`fleet.url "ax:team-a" is not ax:<atespace>/<task>`,
		`fleet.forward[0] "*.agents.svc" is not a glob over http or https URLs`,
		"fleet.interval -1s is negative",
		"fleet.expire -1h0m0s is negative",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}

	cfg.Fleet = fw.FleetConfig{URL: "ftp://host"}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), `fleet.url "ftp://host" is neither`) {
		t.Errorf("ftp URL = %v", err)
	}
}

func TestValidateFleetAccepts(t *testing.T) {
	for _, f := range []fw.FleetConfig{
		{},
		{Subject: "fw.fleet", URL: "ax:team-a/reviewer"},
		{Subject: "fleet", Name: "node7:8080", URL: "http://reviewer.agents.svc:8080", Forward: []string{"http://*.agents.svc:8080", "https://*"}},
	} {
		cfg := fw.DefaultConfig()
		cfg.Fleet = f
		if err := Validate(cfg); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
}

func TestFleetFromTheEnvironment(t *testing.T) {
	cfg := fw.DefaultConfig()
	err := applyEnv(cfg, []string{
		"FW_FLEET_SUBJECT=fw.fleet",
		"FW_FLEET_URL=http://reviewer:8080",
		"FW_FLEET_FORWARD=http://a:*,http://b:*",
		"FW_FLEET_INTERVAL=10s",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := cfg.Fleet
	if f.Subject != "fw.fleet" || f.URL != "http://reviewer:8080" || len(f.Forward) != 2 || f.Forward[1] != "http://b:*" ||
		f.Interval != 10*time.Second || f.Expire != fw.DefaultFleetExpire {
		t.Fatalf("fleet = %+v", f)
	}
}

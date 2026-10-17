package config

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"

	// The binary embeds the zone database. The test embeds it too, so the
	// zone names resolve on a host without zone files.
	_ "time/tzdata"
)

// newAgent builds an agent with in-memory drivers and no real model.
func newAgent(opts ...fw.Option) (*fw.Agent, error) {
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(fw.ModelFunc{}),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	return fw.New(append(base, opts...)...)
}

func TestTimeKeysLoad(t *testing.T) {
	cleanEnv(t)
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), `
time: {zone: America/Chicago, noteAfter: 5m}
agents:
  reviewer: {timeZone: Europe/Berlin}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Time.Zone != "America/Chicago" || cfg.Time.NoteAfter != 5*time.Minute || cfg.Agents["reviewer"].TimeZone != "Europe/Berlin" {
		t.Fatalf("time %+v, reviewer %+v", cfg.Time, cfg.Agents["reviewer"])
	}
	// The variables win over the file.
	t.Setenv("FW_TIME_ZONE", "Asia/Tokyo")
	t.Setenv("FW_TIME_NOTE_AFTER", "0")
	t.Setenv("FW_AGENTS_REVIEWER_TIME_ZONE", "UTC")
	if cfg, err = Load(path); err != nil {
		t.Fatal(err)
	}
	if cfg.Time.Zone != "Asia/Tokyo" || cfg.Time.NoteAfter != 0 || cfg.Agents["reviewer"].TimeZone != "UTC" {
		t.Fatalf("time %+v, reviewer %+v", cfg.Time, cfg.Agents["reviewer"])
	}
}

func TestTimeDefaults(t *testing.T) {
	cleanEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Time.Zone != "" || cfg.Time.NoteAfter != fw.DefaultNoteAfter {
		t.Fatalf("time = %+v", cfg.Time)
	}
	ex, err := Load("../examples/fw.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Time != cfg.Time || ex.Agents["reviewer"].TimeZone != "America/Chicago" {
		t.Errorf("example time %+v, reviewer zone %q", ex.Time, ex.Agents["reviewer"].TimeZone)
	}
}

func TestBadZoneIsNamed(t *testing.T) {
	cleanEnv(t)
	t.Setenv("FW_TIME_ZONE", "Mars/Olympus_Mons")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "time.zone \"Mars/Olympus_Mons\"") {
		t.Errorf("Load with FW_TIME_ZONE: %v", err)
	}
	t.Setenv("FW_TIME_ZONE", "")
	path := writeFile(t, filepath.Join(t.TempDir(), "fw.yaml"), "agents:\n  reviewer: {timeZone: Atlantis/Capital}\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "agents.reviewer.timeZone") {
		t.Errorf("Load with a profile zone: %v", err)
	}
	// fw.New loads the file through ConfigLoader and stops on the same error.
	if _, err := newAgent(fw.WithConfigFile(path)); err == nil || !strings.Contains(err.Error(), "agents.reviewer.timeZone") {
		t.Errorf("New with the file: %v", err)
	}
}

// A library user who builds the configuration in Go skips Load, and fw.New
// still refuses a zone that does not resolve.
func TestNewRefusesBadZone(t *testing.T) {
	cleanEnv(t)
	cfg := fw.DefaultConfig()
	cfg.Time.Zone = "Mars/Olympus_Mons"
	if _, err := newAgent(fw.WithConfig(cfg)); err == nil || !strings.Contains(err.Error(), "time.zone") {
		t.Errorf("New with time.zone: %v", err)
	}
	cfg = fw.DefaultConfig()
	cfg.Agents = map[string]fw.AgentProfile{"reviewer": {TimeZone: "Atlantis/Capital"}}
	if _, err := newAgent(fw.WithConfig(cfg)); err == nil || !strings.Contains(err.Error(), "agents.reviewer.timeZone") {
		t.Errorf("New with a profile zone: %v", err)
	}
	cfg = fw.DefaultConfig()
	cfg.Time.Zone = "America/Chicago"
	a, err := newAgent(fw.WithConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	a.Close(context.Background())
}

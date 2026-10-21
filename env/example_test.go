package env

import (
	"path/filepath"
	"testing"

	"github.com/bdubs00/featherweight-kernel/config"
)

// The block of examples/environment loads through the configuration
// loader, which knows the key because the package claims it.
func TestExampleBlock(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "examples", "environment", "fw.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := load(cfg)
	if err != nil || s == nil {
		t.Fatalf("load = %v, %v", s, err)
	}
	if filepath.Base(s.python) != "pyproject.toml" || filepath.Base(s.tools) != "pixi.toml" || len(s.prepare) != 1 {
		t.Fatalf("spec = %+v", s)
	}
}

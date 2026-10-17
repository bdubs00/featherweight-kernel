package main

// The blank imports select the drivers that the default binary carries.
// Each driver registers itself in init (docs/ARCHITECTURE.md section 12.6).
// The calls below are the only direct uses of a driver package; the
// command line reaches them through the function variables of package cli.
// The workbench builds its own binary from package cli and adds its own
// surfaces, so this list holds only what one agent in one Task needs.

import (
	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/surface/a2a"
	"github.com/bdubs00/featherweight-kernel/telemetry"

	_ "github.com/bdubs00/featherweight-kernel/authn/spiffe"
	_ "github.com/bdubs00/featherweight-kernel/bus/nats"
	_ "github.com/bdubs00/featherweight-kernel/bus/redis"
	_ "github.com/bdubs00/featherweight-kernel/config"
	_ "github.com/bdubs00/featherweight-kernel/model/anthropic"
	_ "github.com/bdubs00/featherweight-kernel/model/gemini"
	_ "github.com/bdubs00/featherweight-kernel/model/openai"
	_ "github.com/bdubs00/featherweight-kernel/surface/metrics"
	_ "github.com/bdubs00/featherweight-kernel/surface/native"
	_ "github.com/bdubs00/featherweight-kernel/tokens"
	_ "github.com/bdubs00/featherweight-kernel/tools/a2a"
	_ "github.com/bdubs00/featherweight-kernel/tools/builtin"
	_ "github.com/bdubs00/featherweight-kernel/tools/mcp"

	// A sandbox image has no zone files, and ax sets no TZ, so the binary
	// carries the zone database (about 450 KB).
	_ "time/tzdata"
)

func init() {
	cli.NewLogger = telemetry.Logger
	cli.SetBuildInfo = telemetry.SetBuildInfo
	cli.CardJSON = a2a.CardJSON
}

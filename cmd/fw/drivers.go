package main

// The blank imports select the drivers that the binary carries. Each driver
// registers itself in init (docs/ARCHITECTURE.md section 12.6). The calls
// below are the only direct uses of a driver package; the command line
// reaches them through the function variables of package cli. The workbench
// builds its own binary from package cli and adds its own surfaces, so this
// list holds only what one agent in one Task needs.
//
// This file holds the core that every build carries: the configuration
// loader, the native API, the built-in tools, the MCP client, the token
// client of the fleet, and the telemetry. The files driver_*.go hold one
// driver each. Without a build tag the binary carries every driver. With
// the tag lean it carries the core alone, and each driver comes back with
// its own tag: nats, redis, anthropic, openai, gemini, spiffe, oidc,
// metrics, a2a, and harness. A worker image for one fleet therefore builds with, for
// example, -tags lean,nats,openai,oidc (section 15).

import (
	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/telemetry"

	_ "github.com/bdubs00/featherweight-kernel/config"
	_ "github.com/bdubs00/featherweight-kernel/surface/native"
	_ "github.com/bdubs00/featherweight-kernel/tokens"
	_ "github.com/bdubs00/featherweight-kernel/tools/builtin"
	_ "github.com/bdubs00/featherweight-kernel/tools/mcp"

	// A sandbox image has no zone files, and ax sets no TZ, so the binary
	// carries the zone database (about 450 KB).
	_ "time/tzdata"
)

func init() {
	cli.NewLogger = telemetry.Logger
	cli.SetBuildInfo = telemetry.SetBuildInfo
}

//go:build !lean || a2a

package main

// The A2A server surface and the A2A tool source, and the agent card that
// fw card prints.

import (
	"github.com/bdubs00/featherweight-kernel/cli"
	"github.com/bdubs00/featherweight-kernel/surface/a2a"

	_ "github.com/bdubs00/featherweight-kernel/tools/a2a"
)

func init() { cli.CardJSON = a2a.CardJSON }

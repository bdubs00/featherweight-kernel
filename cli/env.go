package cli

// This file holds fw env dockerfile, which prints the Dockerfile of the
// block environment (docs/ARCHITECTURE.md section 18).

import (
	"context"
	"errors"
	"io"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// EnvDockerfile writes the Dockerfile of the block environment of cfg,
// FROM the image from, to w. The package env sets it in init. It is nil in
// a binary without that package, such as the lean build without the tag
// env.
var EnvDockerfile func(cfg *fw.Config, from string, w io.Writer) error

// cmdEnvDockerfile prints the Dockerfile of the environment.
func cmdEnvDockerfile(_ context.Context, g *globals, args []string) int {
	fs := g.newFlagSet("env dockerfile", "")
	from := fs.String("from", "", "the base `image`, such as fw:toolbox by digest (required)")
	describe(fs, `Prints a Dockerfile that bakes the block environment of fw.yaml into an
image, for environment.placement: image. It installs uv and pixi with pinned
versions, copies the manifests and the lock files, runs the same commands as
an install at run time with dir inside the image, and writes the marker. It
builds nothing: build the file with the root of the workspace as the
context.`)
	if _, err := parseArgs(fs, args); err != nil {
		return flagExit(err)
	}
	if *from == "" {
		fs.Usage()
		return exitUsage
	}
	if EnvDockerfile == nil {
		return g.fail("env dockerfile", errors.New("the package env is not linked into this binary; build fw without the tag lean, or with the tag env"))
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("env dockerfile", err)
	}
	if err := EnvDockerfile(cfg, *from, g.stdout); err != nil {
		return g.fail("env dockerfile", err)
	}
	return exitOK
}

//go:build !lean || env

package main

// The package environment on the durable volume (the block environment,
// the tool env, and fw env dockerfile).
import _ "github.com/bdubs00/featherweight-kernel/env"

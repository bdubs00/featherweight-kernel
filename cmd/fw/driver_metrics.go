//go:build !lean || metrics

package main

// The surface metrics, a Prometheus endpoint. A worker that pushes its
// metrics over OTLP does not need it.
import _ "github.com/bdubs00/featherweight-kernel/surface/metrics"

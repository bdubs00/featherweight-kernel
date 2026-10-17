// Package metrics serves the Prometheus registry of fw on the shared
// listener. See docs/ARCHITECTURE.md sections 8.5 and 10.1.
//
// The route carries no authentication, because a scraper has no token.
// Keep the route inside the monitoring perimeter. The registry holds the fw
// metrics of the telemetry package, the collectors that linked packages
// register, and the Go runtime and process collectors. See
// examples/observability for a scrape configuration, alert rules, and a
// Grafana dashboard.
package metrics

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/telemetry"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Name is the registered surface name.
const Name = "metrics"

// DefaultPath is the route when Config.Telemetry.Metrics is empty.
const DefaultPath = "/metrics"

func init() {
	fw.RegisterSurface(Name, func(*fw.Config) (fw.Surface, error) { return New(), nil })
}

// Surface serves the registry of one agent. The first Mount builds the
// registry, attaches the metrics observer to the agent, and sets the build
// info. A later Mount, for example a second call to Agent.Handler, adds the
// route to the new mux and reuses the registry.
type Surface struct {
	mu      sync.Mutex
	agent   *fw.Agent
	reg     *prometheus.Registry
	metrics *telemetry.Metrics
}

// New returns the metrics surface.
func New() *Surface { return &Surface{} }

// Mount implements fw.Surface.
func (s *Surface) Mount(mux *http.ServeMux, a *fw.Agent) error {
	path := a.Config().Telemetry.Metrics
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("metrics: telemetry.metrics must be a path that starts with /, not %q", path)
	}
	reg, err := s.setup(a)
	if err != nil {
		return err
	}
	mux.Handle("GET "+path, promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorLog: errorLog{a.Logger()},
	}))
	return nil
}

// setup builds the registry and attaches the observer on the first Mount.
func (s *Surface) setup(a *fw.Agent) (*prometheus.Registry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil {
		if s.agent != a {
			return nil, fmt.Errorf("metrics: the surface is already mounted for another agent")
		}
		return s.reg, nil
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	for _, c := range registeredCollectors() {
		reg.MustRegister(c(a))
	}
	m := telemetry.NewMetrics(reg, a.Prices())
	m.SetBuildInfo(a.Config().Version, "")
	m.KnowModels(modelSelectors(a)...)
	if src, ok := any(a).(toolSourcer); ok {
		m.WatchMCP(src.ToolSources)
	}
	m.Attach(a)
	s.agent, s.reg, s.metrics = a, reg, m
	return reg, nil
}

// toolSourcer is an agent that lists its tool sources. When the agent has
// the method, fw_mcp_server_up follows the MCP servers at each scrape.
type toolSourcer interface {
	ToolSources() []fw.ToolSource
}

// modelSelectors lists the model selectors that the configuration names:
// the default model, the named models, the summary model, and the model of
// each profile. These keep their names as label values.
func modelSelectors(a *fw.Agent) []string {
	cfg := a.Config()
	out := []string{cfg.Model, cfg.ModelSelector(cfg.Compaction.Model)}
	for _, s := range cfg.Models {
		out = append(out, s)
	}
	for _, p := range a.Profiles() {
		out = append(out, cfg.ModelSelector(p.Model))
	}
	return out
}

// Metrics returns the metrics projection, or nil before the first Mount.
// Other packages find the surface in Agent.Surfaces and call, for example,
// SetMCPServerUp on the result.
func (s *Surface) Metrics() *telemetry.Metrics {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metrics
}

// Registry returns the registry, or nil before the first Mount.
func (s *Surface) Registry() *prometheus.Registry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reg
}

// errorLog adapts the agent logger to promhttp.Logger.
type errorLog struct{ log *slog.Logger }

func (l errorLog) Println(v ...any) {
	l.log.Warn("metrics: serve registry", "error", fmt.Sprint(v...))
}

// A CollectorCtor builds a collector for an agent. A linked package adds
// its own series to the registry of this surface by registering one, as
// the fleet of the workbench does for its gauges.
type CollectorCtor func(a *fw.Agent) prometheus.Collector

var collectorCtors struct {
	mu   sync.Mutex
	list []CollectorCtor
}

// RegisterCollector registers a collector constructor. A package calls it
// in init. The surface builds the collector at its first mount for an
// agent.
func RegisterCollector(c CollectorCtor) {
	collectorCtors.mu.Lock()
	defer collectorCtors.mu.Unlock()
	collectorCtors.list = append(collectorCtors.list, c)
}

func registeredCollectors() []CollectorCtor {
	collectorCtors.mu.Lock()
	defer collectorCtors.mu.Unlock()
	return append([]CollectorCtor(nil), collectorCtors.list...)
}

// Package telemetry holds the observability of fw: the Prometheus registry
// that the metrics projection maintains, the OpenTelemetry trace projection,
// and the slog logger. See docs/ARCHITECTURE.md section 10.
//
// Metrics and Tracer are fw.Observer values. Each one folds ledger events
// into its own output and keeps only the state of the runs that are open.
package telemetry

import (
	"context"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// Label values of fw_model_requests_total and fw_tool_calls_total.
const (
	OutcomeOK       = "ok"
	OutcomeError    = "error"
	OutcomeCanceled = "canceled"
	OutcomeDenied   = "denied"
)

// Directions of fw_bus_messages_total.
const (
	DirectionIn  = "in"
	DirectionOut = "out"
)

// unknownLabel replaces a label value that the events cannot supply, or that
// could carry model output, such as the name of a tool that does not exist.
const unknownLabel = "unknown"

// otherLabel replaces a value that the metrics do not keep as a label: an
// origin or a budget limit that this version does not know, or a model name
// past the bound of maxModelLabels.
const otherLabel = "other"

// A run request can name any model in an inline profile, so the model names
// in the events are not bounded by configuration alone. A model that
// KnowModels names always keeps its name. Up to maxModelLabels other
// provider and model pairs keep their names too, and each pair after that
// gets the model label "other". A name longer than maxLabelBytes always
// gets "other".
const (
	maxModelLabels = 32
	maxLabelBytes  = 128
)

// origins are the values of the origin label of fw_runs_queued_total.
var origins = map[string]bool{
	fw.OriginHTTP: true, fw.OriginA2A: true, fw.OriginMCP: true,
	fw.OriginBus: true, fw.OriginCLI: true, fw.OriginTool: true,
}

// limits are the values of the limit label of fw_budget_exceeded_total: the
// budget fields that the kernel names in fw.budget.exceeded.
var limits = map[string]bool{
	"maxTurns": true, "maxTokens": true, "maxCostUSD": true,
	"maxToolCalls": true, "deadline": true, "maxRepeats": true,
}

// Histogram buckets. A model call on a large model can run for minutes, so
// the model buckets reach 900 seconds.
var (
	runDurationBuckets   = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
	runTurnBuckets       = []float64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233}
	modelDurationBuckets = []float64{0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 240, 480, 900}
	toolDurationBuckets  = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	appendBuckets        = []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 1}
)

// Metrics is the metrics projection. It implements fw.Observer and maintains
// the registry of docs/ARCHITECTURE.md section 10.1. No label carries a run
// id, a principal, or user content. The agent, tool, server, provider, and
// model labels hold names from the configuration, and the other labels
// hold values from fixed sets.
type Metrics struct {
	prices fw.Prices

	runsTotal        *prometheus.CounterVec
	runsQueued       *prometheus.CounterVec
	runsActive       *prometheus.GaugeVec
	runDuration      *prometheus.HistogramVec
	runTurns         *prometheus.HistogramVec
	budgetExceeded   *prometheus.CounterVec
	modelRequests    *prometheus.CounterVec
	modelDuration    *prometheus.HistogramVec
	modelTokens      *prometheus.CounterVec
	modelCost        *prometheus.CounterVec
	modelUnpriced    *prometheus.CounterVec
	toolCalls        *prometheus.CounterVec
	toolDuration     *prometheus.HistogramVec
	mcpServerUp      *prometheus.GaugeVec
	approvalsPending *prometheus.GaugeVec
	approvalsOldest  *prometheus.Desc
	approvalsTotal   *prometheus.CounterVec
	loopsTotal       *prometheus.CounterVec
	busMessages      *prometheus.CounterVec
	ledgerEvents     *prometheus.CounterVec
	ledgerAppend     *prometheus.HistogramVec
	buildInfo        *prometheus.GaugeVec

	mu          sync.Mutex
	runs        map[string]*runMetrics // open runs by id
	knownModels map[string]bool        // "provider/model" pairs from KnowModels
	models      map[string]bool        // other pairs that keep their names

	mcpMu      sync.Mutex
	mcpSources func() []fw.ToolSource // see WatchMCP
}

// runMetrics is what Metrics remembers about one open run.
type runMetrics struct {
	agent     string
	batch     string    // the job of the run, from the metadata fw.batch
	active    bool      // counted in fw_runs_active
	since     time.Time // queued, or the first event after the run reopened
	turns     int
	seq       uint64 // events up to this sequence number are in the seeded state
	model     *modelCall
	calls     map[string]string    // tool by call id, for calls that run
	uses      map[string]string    // tool by call id, from the latest response
	known     map[string]bool      // tool names that the kernel resolved
	approvals map[string]time.Time // request time by pending approval id
}

// modelCall is the model call that a run waits for, with the provider and
// the model as the event names them.
type modelCall struct {
	provider, model string
}

// NewMetrics builds the metrics and registers them with reg. A nil reg
// registers nothing. A nil prices leaves fw_model_cost_usd_total at zero
// and counts every token in fw_model_unpriced_tokens_total. NewMetrics
// panics when reg already holds a metric with the same name.
func NewMetrics(reg prometheus.Registerer, prices fw.Prices) *Metrics {
	m := &Metrics{
		prices: prices,
		runsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_runs_total",
			Help: "Runs that finished, by agent, outcome, and batch, the job of the run from the metadata fw.batch, or empty.",
		}, []string{"agent", "outcome", "batch"}),
		runsQueued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_runs_queued_total",
			Help: "Runs that this process queued, by agent, origin (http, a2a, mcp, bus, cli, tool, other), and batch.",
		}, []string{"agent", "origin", "batch"}),
		budgetExceeded: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_budget_exceeded_total",
			Help: "Runs that a budget limit stopped, by agent and limit (maxTurns, maxTokens, maxCostUSD, maxToolCalls, deadline, maxRepeats).",
		}, []string{"agent", "limit"}),
		runsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fw_runs_active",
			Help: "Runs that started and did not finish or suspend.",
		}, []string{"agent"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fw_run_duration_seconds",
			Help:    "Time from the queued event, or from the follow-up that reopened the run, to the finished event.",
			Buckets: runDurationBuckets,
		}, []string{"agent"}),
		runTurns: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fw_run_turns",
			Help:    "Turns of a run at its finished event.",
			Buckets: runTurnBuckets,
		}, []string{"agent"}),
		modelRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_model_requests_total",
			Help: "Model calls, by provider, model, and outcome (ok, error, canceled).",
		}, []string{"provider", "model", "outcome"}),
		modelDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fw_model_request_duration_seconds",
			Help:    "Latency of a model call that returned a response.",
			Buckets: modelDurationBuckets,
		}, []string{"provider", "model"}),
		modelTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_model_tokens_total",
			Help: "Tokens of the model calls, of the summary calls of a compaction, and of the title calls, by provider, model, and kind (input, output, cache_read, cache_write).",
		}, []string{"provider", "model", "kind"}),
		modelCost: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_model_cost_usd_total",
			Help: "Cost in US dollars of the model calls whose model has a price. It leaves out the tokens of fw_model_unpriced_tokens_total.",
		}, []string{"provider", "model"}),
		modelUnpriced: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_model_unpriced_tokens_total",
			Help: "Tokens of every kind of the model calls whose model has no price. Their cost is unknown, not zero.",
		}, []string{"provider", "model"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_tool_calls_total",
			Help: "Tool calls, by tool and outcome (ok, error, denied).",
		}, []string{"tool", "outcome"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fw_tool_call_duration_seconds",
			Help:    "Latency of a tool call that ran.",
			Buckets: toolDurationBuckets,
		}, []string{"tool"}),
		mcpServerUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fw_mcp_server_up",
			Help: "1 when the MCP server answers, else 0.",
		}, []string{"server"}),
		approvalsPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fw_approvals_pending",
			Help: "Approvals that wait for a decision.",
		}, []string{"agent"}),
		approvalsOldest: prometheus.NewDesc("fw_approvals_oldest_timestamp_seconds",
			"Unix time of the request of the oldest approval that waits for a decision, by agent. An agent without such an approval has no series.",
			[]string{"agent"}, nil),
		approvalsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_approvals_total",
			Help: "Approval decisions, by decision (allow, deny).",
		}, []string{"decision"}),
		loopsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_loops_total",
			Help: "Loops of repeated tool calls that the kernel detected, by agent and action (warned, failed).",
		}, []string{"agent", "action"}),
		busMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_bus_messages_total",
			Help: "Bus messages, by driver and direction (in, out).",
		}, []string{"driver", "direction"}),
		ledgerEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "fw_ledger_events_total",
			Help: "Ledger events, by type.",
		}, []string{"type"}),
		ledgerAppend: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "fw_ledger_append_duration_seconds",
			Help:    "Latency of one ledger append.",
			Buckets: appendBuckets,
		}, []string{"driver"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "fw_build_info",
			Help: "Always 1. The labels name the build.",
		}, []string{"version", "commit", "go"}),
		runs:        map[string]*runMetrics{},
		knownModels: map[string]bool{},
		models:      map[string]bool{},
	}
	m.SetBuildInfo("", "")
	if reg != nil {
		reg.MustRegister(m.collectors()...)
	}
	return m
}

func (m *Metrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.runsTotal, m.runsQueued, m.runsActive, m.runDuration, m.runTurns,
		m.budgetExceeded,
		m.modelRequests, m.modelDuration, m.modelTokens, m.modelCost, m.modelUnpriced,
		m.toolCalls, m.toolDuration, mcpCollector{m},
		m.approvalsPending, approvalAge{m}, m.approvalsTotal, m.loopsTotal, m.busMessages,
		m.ledgerEvents, m.ledgerAppend, m.buildInfo,
	}
}

// KnowModels names the model selectors of the configuration, such as
// "anthropic/claude-opus-5-5". These models always keep their names as
// label values. Any other model shares the bound of maxModelLabels.
func (m *Metrics) KnowModels(selectors ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range selectors {
		if p, model := fw.SplitSelector(s); p != "" && model != "" {
			m.knownModels[p+"/"+model] = true
		}
	}
}

// modelLabels returns the label values of a provider and a model. The
// caller holds m.mu.
func (m *Metrics) modelLabels(provider, model string) (string, string) {
	provider, model = labelOr(provider), labelOr(model)
	if len(provider) > maxLabelBytes {
		provider = otherLabel
	}
	key := provider + "/" + model
	switch {
	case m.knownModels[key] || m.models[key]:
		return provider, model
	case len(model) <= maxLabelBytes && len(m.models) < maxModelLabels:
		m.models[key] = true
		return provider, model
	}
	return provider, otherLabel
}

// WatchMCP gives the metrics the tool sources of the agent. At each scrape,
// every source that has the methods Name() string and Up() bool, such as an
// MCP server of the package tools/mcp, sets fw_mcp_server_up for its name.
// SetMCPServerUp still works for a server that is not such a source.
func (m *Metrics) WatchMCP(sources func() []fw.ToolSource) {
	m.mcpMu.Lock()
	defer m.mcpMu.Unlock()
	m.mcpSources = sources
}

// mcpServer is the part of an MCP tool source that fw_mcp_server_up reads.
type mcpServer interface {
	Name() string
	Up() bool
}

// mcpCollector serves fw_mcp_server_up. It reads the sources of WatchMCP
// first, so the gauge shows the state at the time of the scrape.
type mcpCollector struct{ m *Metrics }

func (c mcpCollector) Describe(ch chan<- *prometheus.Desc) { c.m.mcpServerUp.Describe(ch) }

func (c mcpCollector) Collect(ch chan<- prometheus.Metric) {
	c.m.mcpMu.Lock()
	sources := c.m.mcpSources
	c.m.mcpMu.Unlock()
	if sources != nil {
		for _, s := range sources() {
			if srv, ok := s.(mcpServer); ok && srv.Name() != "" {
				c.m.SetMCPServerUp(srv.Name(), srv.Up())
			}
		}
	}
	c.m.mcpServerUp.Collect(ch)
}

// approvalAge serves fw_approvals_oldest_timestamp_seconds from the open
// runs at the time of the scrape.
type approvalAge struct{ m *Metrics }

func (c approvalAge) Describe(ch chan<- *prometheus.Desc) { ch <- c.m.approvalsOldest }

func (c approvalAge) Collect(ch chan<- prometheus.Metric) {
	c.m.mu.Lock()
	oldest := map[string]time.Time{}
	for _, st := range c.m.runs {
		for _, at := range st.approvals {
			if o, ok := oldest[st.agent]; !ok || at.Before(o) {
				oldest[st.agent] = at
			}
		}
	}
	c.m.mu.Unlock()
	for agent, at := range oldest {
		ch <- prometheus.MustNewConstMetric(c.m.approvalsOldest, prometheus.GaugeValue, unixSeconds(at), agent)
	}
}

// unixSeconds is a time as Unix seconds with a fraction.
func unixSeconds(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// build is the identity of the process. It is the one package variable:
// the version and the commit belong to the process, not to one registry,
// and the binary sets them once at start.
var build atomic.Pointer[buildIdent]

type buildIdent struct{ version, commit string }

// SetBuildInfo records the version and the commit of the process. Every
// Metrics that NewMetrics builds afterwards reports them in fw_build_info,
// and NewTracer puts the version in the service.version resource
// attribute. Call it once at start, before the surfaces mount. An empty
// value falls back to what the Go build info records, else "unknown".
func SetBuildInfo(version, commit string) {
	build.Store(&buildIdent{version: version, commit: commit})
}

// SetBuildInfo replaces the one series of fw_build_info of m. An empty
// version or commit takes the process value; see the function SetBuildInfo.
func (m *Metrics) SetBuildInfo(version, commit string) {
	v, c := buildVersion()
	if version == "" {
		version = v
	}
	if commit == "" {
		commit = c
	}
	m.buildInfo.Reset()
	m.buildInfo.WithLabelValues(version, commit, runtime.Version()).Set(1)
}

// buildVersion returns the version and the commit of the process: the
// values of SetBuildInfo, else the main module version and the VCS revision
// that the Go build info records, else "unknown".
func buildVersion() (version, commit string) {
	version, commit = unknownLabel, unknownLabel
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				commit = s.Value
			}
		}
	}
	if b := build.Load(); b != nil {
		if b.version != "" {
			version = b.version
		}
		if b.commit != "" {
			commit = b.commit
		}
	}
	return version, commit
}

// SetMCPServerUp sets fw_mcp_server_up for one MCP server.
func (m *Metrics) SetMCPServerUp(server string, up bool) {
	v := 0.0
	if up {
		v = 1
	}
	m.mcpServerUp.WithLabelValues(server).Set(v)
}

// CountBusMessage adds one message to fw_bus_messages_total. The direction
// is DirectionIn or DirectionOut.
func (m *Metrics) CountBusMessage(driver, direction string) {
	m.busMessages.WithLabelValues(driver, direction).Inc()
}

// ObserveLedgerAppend records the latency of one ledger append.
func (m *Metrics) ObserveLedgerAppend(driver string, d time.Duration) {
	m.ledgerAppend.WithLabelValues(driver).Observe(d.Seconds())
}

// Attach adds m as an observer of a, then counts the runs of a that are
// already open, such as the runs that resumed when a started. Without
// Attach, a run that was open before the observer was added enters
// fw_runs_active at its next event, and its pending approval is not counted.
func (m *Metrics) Attach(a *fw.Agent) {
	a.Observe(m)
	// The lock holds back every event of the agent until the seed is done.
	// The kernel applies an event to the run status before it notifies the
	// observers, so an event that waits here is either in the snapshot or
	// after it, and the per-run state makes both cases count once.
	m.mu.Lock()
	defer m.mu.Unlock()
	runs, err := a.List(context.Background())
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.State.Terminal() || m.runs[r.ID] != nil {
			continue
		}
		st := newRunMetrics(labelOr(r.Agent))
		st.since, st.turns, st.seq = r.CreatedAt, r.Usage.Turns, r.Seq
		if r.State == fw.StateRunning || r.State == fw.StateInputRequired {
			st.active = true
			m.runsActive.WithLabelValues(st.agent).Inc()
		}
		if r.Pending != nil {
			at := r.Pending.RequestedAt
			if at.IsZero() {
				at = time.Now()
			}
			st.approvals[r.Pending.ID] = at
			m.approvalsPending.WithLabelValues(st.agent).Inc()
		}
		m.runs[r.ID] = st
	}
}

func newRunMetrics(agent string) *runMetrics {
	return &runMetrics{
		agent:     agent,
		calls:     map[string]string{},
		uses:      map[string]string{},
		known:     map[string]bool{},
		approvals: map[string]time.Time{},
	}
}

// Observe implements fw.Observer.
func (m *Metrics) Observe(e fw.Event) {
	m.ledgerEvents.WithLabelValues(e.Type).Inc()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.runs[e.Subject]
	switch e.Type {
	case fw.EvRunQueued:
		if st == nil {
			st = newRunMetrics(agentOf(e.Source))
			m.runs[e.Subject] = st
		}
		st.since = eventTime(e)
		var d struct {
			Origin   string            `json:"origin"`
			Metadata map[string]string `json:"metadata"`
		}
		_ = e.Decode(&d)
		origin := d.Origin
		if !origins[origin] {
			origin = otherLabel
		}
		st.batch = d.Metadata[fw.MetaBatch]
		m.runsQueued.WithLabelValues(st.agent, origin, st.batch).Inc()
		return
	case fw.EvRunFinished:
		m.finish(e, st)
		return
	case fw.EvRunSuspended:
		m.suspend(e, st)
		return
	case fw.EvRunAnnotated:
		// An annotation can arrive after the run finished. It counts the
		// tokens of the call that wrote it and never opens the run.
		var d fw.RunAnnotatedData
		if e.Decode(&d) == nil && d.Usage != nil {
			m.usage(d.Provider, d.Model, *d.Usage)
		}
		return
	case fw.EvRunChild, fw.EvChildReported:
		// The record and the report of a child can arrive after its parent
		// finished, and they never open the parent. The child counts its
		// own usage.
		return
	}
	// Every other event shows that the run is open: fw.run.started, a
	// resume, or a follow-up that reopened a finished run.
	if st == nil {
		st = newRunMetrics(agentOf(e.Source))
		st.since = eventTime(e)
		m.runs[e.Subject] = st
	}
	if !st.active {
		st.active = true
		m.runsActive.WithLabelValues(st.agent).Inc()
	}
	switch e.Type {
	case fw.EvRunStarted:
		var d fw.RunStartedData
		if e.Decode(&d) == nil {
			for _, t := range d.Tools {
				st.known[t] = true
			}
		}
	case fw.EvTurnStarted:
		if e.Seq == 0 || e.Seq > st.seq {
			st.turns++
		}
	case fw.EvModelRequested:
		var d fw.ModelRequestedData
		if e.Decode(&d) == nil {
			st.model = &modelCall{provider: d.Provider, model: d.Model}
		}
	case fw.EvModelResponded:
		m.responded(e, st)
	case fw.EvContextCompacted:
		var d struct {
			Usage    *fw.Usage `json:"usage"`
			Provider string    `json:"provider"`
			Model    string    `json:"model"`
		}
		if e.Decode(&d) == nil && d.Usage != nil {
			m.usage(d.Provider, d.Model, *d.Usage)
		}
	case fw.EvError:
		if st.model != nil {
			m.countModelEnd(st.model, OutcomeError)
			st.model = nil
		}
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) != nil {
			return
		}
		limit := d.Limit
		if !limits[limit] {
			limit = otherLabel
		}
		m.budgetExceeded.WithLabelValues(st.agent, limit).Inc()
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) == nil {
			st.calls[d.CallID] = labelOr(d.Tool)
			st.known[d.Tool] = true
		}
	case fw.EvToolResponded:
		m.toolResponded(e, st)
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) != nil {
			return
		}
		if d.Tool != "" {
			st.known[d.Tool] = true
		}
		if _, ok := st.approvals[d.ApprovalID]; !ok {
			st.approvals[d.ApprovalID] = eventTime(e)
			m.approvalsPending.WithLabelValues(st.agent).Inc()
		}
	case fw.EvLoopDetected:
		var d fw.LoopDetectedData
		if e.Decode(&d) != nil {
			return
		}
		action := d.Action
		if action != "warned" && action != "failed" {
			action = unknownLabel
		}
		m.loopsTotal.WithLabelValues(st.agent, action).Inc()
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) != nil {
			return
		}
		m.approvalsTotal.WithLabelValues(decisionLabel(d.Decision)).Inc()
		if _, ok := st.approvals[d.ApprovalID]; ok {
			delete(st.approvals, d.ApprovalID)
			m.approvalsPending.WithLabelValues(st.agent).Dec()
		}
	}
}

// countModelEnd counts a model call that ended without a response.
func (m *Metrics) countModelEnd(c *modelCall, outcome string) {
	provider, model := m.modelLabels(c.provider, c.model)
	m.modelRequests.WithLabelValues(provider, model, outcome).Inc()
}

// usage counts the tokens of a model usage. With a price, it adds the cost
// to fw_model_cost_usd_total. Without a price, it adds the tokens to
// fw_model_unpriced_tokens_total, so a dashboard can tell an unknown cost
// from a cost of zero. A price of zero is a price.
func (m *Metrics) usage(provider, model string, u fw.Usage) {
	lp, lm := m.modelLabels(provider, model)
	m.modelTokens.WithLabelValues(lp, lm, "input").Add(float64(u.InputTokens))
	m.modelTokens.WithLabelValues(lp, lm, "output").Add(float64(u.OutputTokens))
	m.modelTokens.WithLabelValues(lp, lm, "cache_read").Add(float64(u.CacheReadTokens))
	m.modelTokens.WithLabelValues(lp, lm, "cache_write").Add(float64(u.CacheWriteTokens))
	if p, ok := m.price(provider, model); ok {
		m.modelCost.WithLabelValues(lp, lm).Add(fw.Cost(u, p))
		return
	}
	if n := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens; n > 0 {
		m.modelUnpriced.WithLabelValues(lp, lm).Add(float64(n))
	}
}

// responded counts one model response: the request, its latency, its
// tokens, and its cost.
func (m *Metrics) responded(e fw.Event, st *runMetrics) {
	var d struct {
		Usage     fw.Usage `json:"usage"`
		LatencyMS int64    `json:"latencyMs"`
		Provider  string   `json:"provider"`
		Model     string   `json:"model"`
		Content   []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if e.Decode(&d) != nil {
		return
	}
	provider, model := d.Provider, d.Model
	if st.model != nil {
		if provider == "" {
			provider = st.model.provider
		}
		if model == "" {
			model = st.model.model
		}
	}
	st.model = nil
	lp, lm := m.modelLabels(provider, model)
	m.modelRequests.WithLabelValues(lp, lm, OutcomeOK).Inc()
	m.modelDuration.WithLabelValues(lp, lm).Observe(float64(d.LatencyMS) / 1000)
	m.usage(provider, model, d.Usage)
	clear(st.uses)
	for _, b := range d.Content {
		if b.Type == fw.BlockToolUse {
			st.uses[b.ID] = b.Name
		}
	}
}

// toolResponded counts one tool result. A call that ran has a
// fw.tool.requested event. A call that did not run was refused by the
// policy or by a decision, or named a tool that does not exist.
func (m *Metrics) toolResponded(e fw.Event, st *runMetrics) {
	var d fw.ToolRespondedData
	if e.Decode(&d) != nil {
		return
	}
	if tool, ok := st.calls[d.CallID]; ok {
		delete(st.calls, d.CallID)
		outcome := OutcomeOK
		if d.IsError {
			outcome = OutcomeError
		}
		m.toolCalls.WithLabelValues(tool, outcome).Inc()
		m.toolDuration.WithLabelValues(tool).Observe(float64(d.LatencyMS) / 1000)
		return
	}
	// The name in a tool_use block comes from the model. It becomes a label
	// only when the kernel resolved it to a tool.
	if name := st.uses[d.CallID]; name != "" && st.known[name] {
		m.toolCalls.WithLabelValues(name, OutcomeDenied).Inc()
		return
	}
	m.toolCalls.WithLabelValues(unknownLabel, OutcomeError).Inc()
}

// finish counts a finished run and forgets it.
func (m *Metrics) finish(e fw.Event, st *runMetrics) {
	var d fw.RunFinishedData
	_ = e.Decode(&d)
	outcome := d.Outcome
	switch outcome {
	case fw.OutcomeCompleted, fw.OutcomeCanceled, fw.OutcomeFailed:
	default:
		outcome = fw.OutcomeFailed
	}
	agent, batch := agentOf(e.Source), ""
	if st != nil {
		agent, batch = st.agent, st.batch
	}
	m.runsTotal.WithLabelValues(agent, outcome, batch).Inc()
	if st == nil {
		return
	}
	if st.model != nil {
		mo := OutcomeError
		if outcome == fw.OutcomeCanceled {
			mo = OutcomeCanceled
		}
		m.countModelEnd(st.model, mo)
	}
	if !st.since.IsZero() {
		if dur := eventTime(e).Sub(st.since); dur >= 0 {
			m.runDuration.WithLabelValues(agent).Observe(dur.Seconds())
		}
	}
	m.runTurns.WithLabelValues(agent).Observe(float64(st.turns))
	if st.active {
		m.runsActive.WithLabelValues(agent).Dec()
	}
	// A finished run waits for nothing.
	for range st.approvals {
		m.approvalsPending.WithLabelValues(agent).Dec()
	}
	delete(m.runs, e.Subject)
}

// suspend takes a run out of fw_runs_active. Its pending approvals stay
// pending, because a resumed run still waits for them.
func (m *Metrics) suspend(e fw.Event, st *runMetrics) {
	if st == nil {
		return
	}
	if st.model != nil {
		m.countModelEnd(st.model, OutcomeCanceled)
		st.model = nil
	}
	clear(st.calls)
	if st.active {
		st.active = false
		m.runsActive.WithLabelValues(st.agent).Dec()
	}
	if len(st.approvals) == 0 {
		delete(m.runs, e.Subject)
	}
}

// price looks up "provider/model", then the model alone, as the kernel does.
func (m *Metrics) price(provider, model string) (fw.Price, bool) {
	if m.prices == nil || model == "" {
		return fw.Price{}, false
	}
	if provider != "" {
		if p, ok := m.prices.Price(provider + "/" + model); ok {
			return p, true
		}
	}
	return m.prices.Price(model)
}

func decisionLabel(d string) string {
	switch d {
	case "allow", "deny":
		return d
	}
	return "other"
}

func labelOr(v string) string {
	if v == "" {
		return unknownLabel
	}
	return v
}

// agentOf returns the agent of a source "fw://<atespace>/<agent>/<run>".
// It matches the rule that the kernel uses for bus subjects.
func agentOf(src string) string {
	rest, ok := strings.CutPrefix(src, "fw://")
	if !ok {
		return unknownLabel
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return unknownLabel
	}
	return labelOr(strings.Join(parts[1:len(parts)-1], "/"))
}

// eventTime is the time of an event, or now when the event has none.
func eventTime(e fw.Event) time.Time {
	if e.Time.IsZero() {
		return time.Now()
	}
	return e.Time
}

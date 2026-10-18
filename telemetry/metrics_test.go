package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// feed builds the events of one run with growing sequence numbers and
// times, one second apart.
type feed struct {
	obs fw.Observer
	run string
	src string
	tp  string
	seq uint64
	at  time.Time
}

func newFeed(obs fw.Observer, run, agent string) *feed {
	return &feed{
		obs: obs,
		run: run,
		src: "fw://local/" + agent + "/" + run,
		tp:  "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		at:  time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC),
	}
}

func (f *feed) emit(typ string, data any) fw.Event {
	f.seq++
	f.at = f.at.Add(time.Second)
	e := fw.MustEvent(f.run, typ, data)
	e.Source, e.Seq, e.Time, e.TraceParent = f.src, f.seq, f.at, f.tp
	f.obs.Observe(e)
	return e
}

func toolUse(id, name string) fw.Block {
	return fw.Block{Type: fw.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(`{}`)}
}

// value returns the value of a counter or a gauge, or the sample count of a
// histogram, for the series with exactly these labels.
func value(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	v, ok := lookup(t, g, name, labels)
	if !ok {
		t.Fatalf("no series %s%v", name, labels)
	}
	return v
}

func lookup(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !sameLabels(m.GetLabel(), labels) {
				continue
			}
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				return m.GetCounter().GetValue(), true
			case dto.MetricType_GAUGE:
				return m.GetGauge().GetValue(), true
			case dto.MetricType_HISTOGRAM:
				return float64(m.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}

func histogramSum(t *testing.T, g prometheus.Gatherer, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if mf.GetName() == name && sameLabels(m.GetLabel(), labels) {
				return m.GetHistogram().GetSampleSum()
			}
		}
	}
	t.Fatalf("no histogram %s%v", name, labels)
	return 0
}

func sameLabels(got []*dto.LabelPair, want map[string]string) bool {
	// The run counters carry the label batch, empty for a run of no job. An
	// expectation that leaves it out means empty.
	if _, ok := want["batch"]; !ok {
		var kept []*dto.LabelPair
		for _, l := range got {
			if l.GetName() == "batch" && l.GetValue() == "" {
				continue
			}
			kept = append(kept, l)
		}
		got = kept
	}
	if len(got) != len(want) {
		return false
	}
	for _, l := range got {
		if want[l.GetName()] != l.GetValue() {
			return false
		}
	}
	return true
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestRegistryNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	// Touch every vector so that each family appears in the output.
	m.runsTotal.WithLabelValues("a", "completed", "")
	m.runsQueued.WithLabelValues("a", fw.OriginHTTP, "")
	m.budgetExceeded.WithLabelValues("a", "maxTurns")
	m.modelUnpriced.WithLabelValues("p", "m")
	m.loopsTotal.WithLabelValues("a", "warned")
	m.runsActive.WithLabelValues("a")
	m.runDuration.WithLabelValues("a")
	m.runTurns.WithLabelValues("a")
	m.modelRequests.WithLabelValues("p", "m", "ok")
	m.modelDuration.WithLabelValues("p", "m")
	m.modelTokens.WithLabelValues("p", "m", "input")
	m.modelCost.WithLabelValues("p", "m")
	m.toolCalls.WithLabelValues("t", "ok")
	m.toolDuration.WithLabelValues("t")
	m.SetMCPServerUp("git", true)
	m.approvalsPending.WithLabelValues("a")
	m.approvalsTotal.WithLabelValues("allow")
	m.CountBusMessage("nats", DirectionIn)
	m.ledgerEvents.WithLabelValues(fw.EvRunQueued)
	m.ObserveLedgerAppend("file", time.Millisecond)
	f := newFeed(m, "r1", "a")
	f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "rm"})
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		typ    dto.MetricType
		labels []string
	}{
		"fw_runs_total":                         {dto.MetricType_COUNTER, []string{"agent", "batch", "outcome"}},
		"fw_runs_queued_total":                  {dto.MetricType_COUNTER, []string{"agent", "batch", "origin"}},
		"fw_budget_exceeded_total":              {dto.MetricType_COUNTER, []string{"agent", "limit"}},
		"fw_model_unpriced_tokens_total":        {dto.MetricType_COUNTER, []string{"model", "provider"}},
		"fw_approvals_oldest_timestamp_seconds": {dto.MetricType_GAUGE, []string{"agent"}},
		"fw_loops_total":                        {dto.MetricType_COUNTER, []string{"action", "agent"}},
		"fw_runs_active":                        {dto.MetricType_GAUGE, []string{"agent"}},
		"fw_run_duration_seconds":               {dto.MetricType_HISTOGRAM, []string{"agent"}},
		"fw_run_turns":                          {dto.MetricType_HISTOGRAM, []string{"agent"}},
		"fw_model_requests_total":               {dto.MetricType_COUNTER, []string{"model", "outcome", "provider"}},
		"fw_model_request_duration_seconds":     {dto.MetricType_HISTOGRAM, []string{"model", "provider"}},
		"fw_model_tokens_total":                 {dto.MetricType_COUNTER, []string{"kind", "model", "provider"}},
		"fw_model_cost_usd_total":               {dto.MetricType_COUNTER, []string{"model", "provider"}},
		"fw_tool_calls_total":                   {dto.MetricType_COUNTER, []string{"outcome", "tool"}},
		"fw_tool_call_duration_seconds":         {dto.MetricType_HISTOGRAM, []string{"tool"}},
		"fw_mcp_server_up":                      {dto.MetricType_GAUGE, []string{"server"}},
		"fw_approvals_pending":                  {dto.MetricType_GAUGE, []string{"agent"}},
		"fw_approvals_total":                    {dto.MetricType_COUNTER, []string{"decision"}},
		"fw_bus_messages_total":                 {dto.MetricType_COUNTER, []string{"direction", "driver"}},
		"fw_ledger_events_total":                {dto.MetricType_COUNTER, []string{"type"}},
		"fw_ledger_append_duration_seconds":     {dto.MetricType_HISTOGRAM, []string{"driver"}},
		"fw_build_info":                         {dto.MetricType_GAUGE, []string{"commit", "go", "version"}},
	}
	if len(mfs) != len(want) {
		t.Fatalf("registry has %d families, want %d", len(mfs), len(want))
	}
	for _, mf := range mfs {
		w, ok := want[mf.GetName()]
		if !ok || mf.GetType() != w.typ {
			t.Fatalf("family %s of type %s is not expected", mf.GetName(), mf.GetType())
		}
		var names []string
		for _, l := range mf.GetMetric()[0].GetLabel() {
			names = append(names, l.GetName())
		}
		if len(names) != len(w.labels) {
			t.Fatalf("%s labels = %v, want %v", mf.GetName(), names, w.labels)
		}
		for i := range names {
			if names[i] != w.labels[i] {
				t.Fatalf("%s labels = %v, want %v", mf.GetName(), names, w.labels)
			}
		}
	}
	if b := m.modelDuration.WithLabelValues("p", "m"); b == nil || modelDurationBuckets[len(modelDurationBuckets)-1] != 900 {
		t.Fatal("the model latency buckets must reach 900 seconds")
	}
}

func TestMetricsRun(t *testing.T) {
	reg := prometheus.NewRegistry()
	prices := fw.PriceTable{"anthropic/claude-x": {Input: 3, Output: 15}}
	m := NewMetrics(reg, prices)
	f := newFeed(m, "01RUNID", "reviewer")
	agent := map[string]string{"agent": "reviewer"}

	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "secret input", Origin: fw.OriginMCP, Principal: "alice@example.com"})
	if v, ok := lookup(t, reg, "fw_runs_active", agent); ok && v != 0 {
		t.Fatalf("a queued run is active: %v", v)
	}
	f.emit(fw.EvRunStarted, fw.RunStartedData{Model: "anthropic/claude-x", Tools: []string{"echo", "rm"}})
	if v := value(t, reg, "fw_runs_active", agent); v != 1 {
		t.Fatalf("active = %v", v)
	}
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{
		Content:    []fw.Block{fw.Text("x"), toolUse("c1", "echo"), toolUse("c2", "rm"), toolUse("c3", "made up name")},
		StopReason: fw.StopToolUse,
		Usage:      fw.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 30, CacheWriteTokens: 20},
		LatencyMS:  2000,
		Provider:   "anthropic",
		Model:      "claude-x",
	})
	f.emit(fw.EvToolRequested, fw.ToolRequestedData{Tool: "echo", CallID: "c1"})
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c1", LatencyMS: 500})
	asked := f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "ap1", Tool: "rm", CallID: "c2"})
	if v := value(t, reg, "fw_approvals_pending", agent); v != 1 {
		t.Fatalf("pending = %v", v)
	}
	if v := value(t, reg, "fw_approvals_oldest_timestamp_seconds", agent); v != float64(asked.Time.Unix()) {
		t.Fatalf("oldest approval = %v, want %v", v, asked.Time.Unix())
	}
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "ap1", Decision: "deny", Decider: "alice@example.com"})
	if v := value(t, reg, "fw_approvals_pending", agent); v != 0 {
		t.Fatalf("pending after decision = %v", v)
	}
	if _, ok := lookup(t, reg, "fw_approvals_oldest_timestamp_seconds", agent); ok {
		t.Fatal("a decided approval still has an age")
	}
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c2", IsError: true})
	f.emit(fw.EvToolResponded, fw.ToolRespondedData{CallID: "c3", IsError: true})
	f.emit(fw.EvTurnFinished, fw.TurnData{Turn: 1})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 2})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{
		Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn,
		Usage: fw.Usage{InputTokens: 10, OutputTokens: 5}, LatencyMS: 1000, Provider: "anthropic", Model: "claude-x",
	})
	f.emit(fw.EvTurnFinished, fw.TurnData{Turn: 2})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted, Output: "secret output"})

	model := map[string]string{"provider": "anthropic", "model": "claude-x"}
	with := func(base map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for bk, bv := range base {
			out[bk] = bv
		}
		return out
	}
	checks := []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_runs_total", with(agent, "outcome", "completed"), 1},
		{"fw_runs_queued_total", with(agent, "origin", "mcp"), 1},
		{"fw_runs_active", agent, 0},
		{"fw_run_turns", agent, 1},
		{"fw_run_duration_seconds", agent, 1},
		{"fw_model_requests_total", with(model, "outcome", "ok"), 2},
		{"fw_model_request_duration_seconds", model, 2},
		{"fw_model_tokens_total", with(model, "kind", "input"), 110},
		{"fw_model_tokens_total", with(model, "kind", "output"), 55},
		{"fw_model_tokens_total", with(model, "kind", "cache_read"), 30},
		{"fw_model_tokens_total", with(model, "kind", "cache_write"), 20},
		{"fw_tool_calls_total", map[string]string{"tool": "echo", "outcome": "ok"}, 1},
		{"fw_tool_calls_total", map[string]string{"tool": "rm", "outcome": "denied"}, 1},
		{"fw_tool_calls_total", map[string]string{"tool": "unknown", "outcome": "error"}, 1},
		{"fw_tool_call_duration_seconds", map[string]string{"tool": "echo"}, 1},
		{"fw_approvals_pending", agent, 0},
		{"fw_approvals_total", map[string]string{"decision": "deny"}, 1},
		{"fw_ledger_events_total", map[string]string{"type": fw.EvToolResponded}, 3},
		{"fw_ledger_events_total", map[string]string{"type": fw.EvRunFinished}, 1},
	}
	for _, c := range checks {
		if got := value(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	// (150 input-side tokens * 3 + 50 * 15) + (10 * 3 + 5 * 15), per million.
	if got := value(t, reg, "fw_model_cost_usd_total", model); !near(got, 1305.0/1_000_000) {
		t.Errorf("cost = %v", got)
	}
	if _, ok := lookup(t, reg, "fw_model_unpriced_tokens_total", model); ok {
		t.Error("a priced model has unpriced tokens")
	}
	if got := histogramSum(t, reg, "fw_run_turns", agent); got != 2 {
		t.Errorf("turns sum = %v", got)
	}
	if got := histogramSum(t, reg, "fw_model_request_duration_seconds", model); got != 3 {
		t.Errorf("model latency sum = %v", got)
	}
	// From queued to finished: 17 events, one second apart.
	if got := histogramSum(t, reg, "fw_run_duration_seconds", agent); got != 16 {
		t.Errorf("run duration sum = %v", got)
	}
	// No label carries the run id or user content.
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, mt := range mf.GetMetric() {
			for _, l := range mt.GetLabel() {
				switch l.GetValue() {
				case f.run, "secret input", "secret output", "made up name", "alice@example.com":
					t.Errorf("%s has label %s=%q", mf.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}
	m.mu.Lock()
	open := len(m.runs)
	m.mu.Unlock()
	if open != 0 {
		t.Fatalf("metrics remember %d finished runs", open)
	}
}

func TestMetricsFailuresAndCancel(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	model := map[string]string{"provider": "openai", "model": "gpt-x"}

	f := newFeed(m, "r1", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "x"})
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "openai", Model: "gpt-x"})
	f.emit(fw.EvError, fw.ErrorData{Message: "model: boom"})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "model: boom"})

	g := newFeed(m, "r2", "default")
	g.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "x"})
	g.emit(fw.EvRunStarted, fw.RunStartedData{})
	g.emit(fw.EvTurnStarted, fw.TurnData{Turn: 1})
	g.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "openai", Model: "gpt-x"})
	g.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "q1", Tool: "ask"})
	g.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCanceled})

	// A run that this process never saw start finishes: the counter moves,
	// and no gauge goes below zero.
	h := newFeed(m, "r3", "default")
	h.seq = 40
	h.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})

	// A follow-up reopens a finished run.
	f.emit(fw.EvMessageReceived, fw.MessageReceivedData{Role: fw.RoleUser, Content: []fw.Block{fw.Text("again")}, Origin: fw.OriginHTTP})
	if v := value(t, reg, "fw_runs_active", map[string]string{"agent": "default"}); v != 1 {
		t.Fatalf("active after follow-up = %v", v)
	}
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})

	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_model_requests_total", map[string]string{"provider": "openai", "model": "gpt-x", "outcome": "error"}, 1},
		{"fw_model_requests_total", map[string]string{"provider": "openai", "model": "gpt-x", "outcome": "canceled"}, 1},
		{"fw_runs_total", map[string]string{"agent": "default", "outcome": "failed"}, 1},
		{"fw_runs_total", map[string]string{"agent": "default", "outcome": "canceled"}, 1},
		{"fw_runs_total", map[string]string{"agent": "default", "outcome": "completed"}, 2},
		{"fw_runs_active", map[string]string{"agent": "default"}, 0},
		{"fw_approvals_pending", map[string]string{"agent": "default"}, 0},
	} {
		if got := value(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if _, ok := lookup(t, reg, "fw_model_request_duration_seconds", model); ok {
		t.Error("a failed model call has a latency sample")
	}
}

func TestMetricsSuspend(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	agent := map[string]string{"agent": "default"}
	f := newFeed(m, "r1", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvApprovalRequested, fw.ApprovalRequestedData{ApprovalID: "a1", Tool: "rm"})
	f.emit(fw.EvRunSuspended, fw.RunSuspendedData{Reason: "shutdown"})
	if value(t, reg, "fw_runs_active", agent) != 0 || value(t, reg, "fw_approvals_pending", agent) != 1 {
		t.Fatal("a suspended run is active, or its approval stopped pending")
	}
	f.emit(fw.EvRunResumed, fw.RunResumedData{FromSeq: 3})
	f.emit(fw.EvApprovalDecided, fw.ApprovalDecidedData{ApprovalID: "a1", Decision: "allow"})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	if value(t, reg, "fw_runs_active", agent) != 0 || value(t, reg, "fw_approvals_pending", agent) != 0 ||
		value(t, reg, "fw_approvals_total", map[string]string{"decision": "allow"}) != 1 {
		t.Fatal("gauges did not return to zero")
	}
}

func TestBuildInfoAndMCP(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.SetBuildInfo("1.2.3", "abc123")
	if v := value(t, reg, "fw_build_info", map[string]string{"version": "1.2.3", "commit": "abc123", "go": runtime.Version()}); v != 1 {
		t.Fatalf("build info = %v", v)
	}
	if n, _ := countSeries(reg, "fw_build_info"); n != 1 {
		t.Fatalf("build info has %d series", n)
	}
	m.SetMCPServerUp("git", true)
	m.SetMCPServerUp("jira", false)
	if value(t, reg, "fw_mcp_server_up", map[string]string{"server": "git"}) != 1 || value(t, reg, "fw_mcp_server_up", map[string]string{"server": "jira"}) != 0 {
		t.Fatal("mcp server gauge")
	}
	m.SetMCPServerUp("git", false)
	if value(t, reg, "fw_mcp_server_up", map[string]string{"server": "git"}) != 0 {
		t.Fatal("mcp server gauge did not drop")
	}
	m.CountBusMessage("nats", DirectionOut)
	m.ObserveLedgerAppend("file", 2*time.Millisecond)
	if value(t, reg, "fw_bus_messages_total", map[string]string{"driver": "nats", "direction": "out"}) != 1 ||
		value(t, reg, "fw_ledger_append_duration_seconds", map[string]string{"driver": "file"}) != 1 {
		t.Fatal("bus or ledger metric")
	}
}

func TestProcessBuildInfo(t *testing.T) {
	t.Cleanup(func() { build.Store(nil) })
	SetBuildInfo("7.7.7", "deadbeef")
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	labels := map[string]string{"version": "7.7.7", "commit": "deadbeef", "go": runtime.Version()}
	if v := value(t, reg, "fw_build_info", labels); v != 1 {
		t.Fatalf("build info = %v", v)
	}
	// The method overrides one registry; an empty value keeps the process value.
	m.SetBuildInfo("8.0.0", "")
	labels["version"] = "8.0.0"
	if v := value(t, reg, "fw_build_info", labels); v != 1 {
		t.Fatalf("build info after the method = %v", v)
	}
}

func countSeries(g prometheus.Gatherer, name string) (int, error) {
	mfs, err := g.Gather()
	if err != nil {
		return 0, err
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return len(mf.GetMetric()), nil
		}
	}
	return 0, nil
}

// script returns canned responses in order.
type script struct {
	mu    sync.Mutex
	resps []*fw.Response
}

func (s *script) model() fw.Model {
	return fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.resps) == 0 {
			return nil, errors.New("script: no response left")
		}
		r := s.resps[0]
		s.resps = s.resps[1:]
		return r, nil
	}}
}

func newAgent(t *testing.T, m fw.Model, opts ...fw.Option) *fw.Agent {
	t.Helper()
	t.Setenv("FW_ATESPACE", "")
	base := []fw.Option{
		fw.WithoutDefaults(),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModelName("anthropic/claude-opus-5-5"),
		fw.WithModel(m),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
	a, err := fw.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.Close(ctx)
	})
	return a
}

// TestAttachCountsOpenRuns runs a real agent. The run waits for an
// approval before the metrics exist, so only Attach can count it.
func TestAttachCountsOpenRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rm := fw.NewTool(fw.ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("removed"), nil
	})
	s := &script{resps: []*fw.Response{
		{Content: []fw.Block{toolUse("c1", "rm")}, StopReason: fw.StopToolUse, Usage: fw.Usage{InputTokens: 7, OutputTokens: 3}},
		{Content: []fw.Block{fw.Text("done")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 9, OutputTokens: 4}},
	}}
	a := newAgent(t, s.model(), fw.WithTools(rm))
	r, err := a.Run(ctx, "clean up")
	if err != nil || r.State != fw.StateInputRequired {
		t.Fatalf("run = %+v, %v", r, err)
	}
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.Attach(a)
	agent := map[string]string{"agent": "default"}
	if value(t, reg, "fw_runs_active", agent) != 1 || value(t, reg, "fw_approvals_pending", agent) != 1 {
		t.Fatal("Attach did not count the open run")
	}
	if got, want := value(t, reg, "fw_approvals_oldest_timestamp_seconds", agent), unixSeconds(r.Pending.RequestedAt); !near(got, want) {
		t.Fatalf("oldest approval = %v, want %v", got, want)
	}
	if err := a.Decide(ctx, r.ID, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	if r, err = a.Wait(ctx, r.ID); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("run = %+v, %v", r, err)
	}
	// The kernel notifies observers after Wait sees the terminal state.
	eventually(t, func() bool {
		_, ok := lookup(t, reg, "fw_runs_total", map[string]string{"agent": "default", "outcome": "completed"})
		return ok
	})
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_runs_active", agent, 0},
		{"fw_approvals_pending", agent, 0},
		{"fw_runs_total", map[string]string{"agent": "default", "outcome": "completed"}, 1},
		{"fw_tool_calls_total", map[string]string{"tool": "rm", "outcome": "ok"}, 1},
		{"fw_approvals_total", map[string]string{"decision": "allow"}, 1},
		{"fw_model_requests_total", map[string]string{"provider": "anthropic", "model": "claude-opus-5-5", "outcome": "ok"}, 1},
		{"fw_model_tokens_total", map[string]string{"provider": "anthropic", "model": "claude-opus-5-5", "kind": "output"}, 4},
		{"fw_run_turns", agent, 1},
	} {
		if got := value(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	// The run had one turn before Attach and one after: the seed counts the
	// first, and the observer counts the second once.
	if got := histogramSum(t, reg, "fw_run_turns", agent); got != 2 {
		t.Errorf("turns sum = %v", got)
	}
}

func TestMetricsUnpricedAndCompaction(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, fw.PriceTable{"anthropic/claude-x": {Input: 3, Output: 15}, "local/free": {}})
	f := newFeed(m, "r1", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	respond := func(provider, model string, u fw.Usage) {
		f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: provider, Model: model})
		f.emit(fw.EvModelResponded, fw.ModelRespondedData{Content: []fw.Block{fw.Text("x")}, StopReason: fw.StopEndTurn, Usage: u, Provider: provider, Model: model})
	}
	respond("openai", "gpt-x", fw.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CacheWriteTokens: 1})
	respond("local", "free", fw.Usage{InputTokens: 7, OutputTokens: 3})
	// A compaction counts the tokens of its summary calls, as the kernel does.
	f.emit(fw.EvContextCompacted, fw.ContextCompactedData{Epoch: 1, Summary: fw.Text("summary"),
		Usage: &fw.Usage{InputTokens: 1000, OutputTokens: 100}, Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvContextCompacted, fw.ContextCompactedData{Epoch: 2, Summary: fw.Text("summary"),
		Usage: &fw.Usage{InputTokens: 50, OutputTokens: 10}, Provider: "openai", Model: "gpt-x"})

	gpt := map[string]string{"provider": "openai", "model": "gpt-x"}
	claude := map[string]string{"provider": "anthropic", "model": "claude-x"}
	free := map[string]string{"provider": "local", "model": "free"}
	if got := value(t, reg, "fw_model_unpriced_tokens_total", gpt); got != 186 {
		t.Errorf("unpriced gpt tokens = %v, want 186", got)
	}
	if _, ok := lookup(t, reg, "fw_model_cost_usd_total", gpt); ok {
		t.Error("an unpriced model has a cost series")
	}
	// A price of zero is a known cost of zero, not an unknown cost.
	if got := value(t, reg, "fw_model_cost_usd_total", free); got != 0 {
		t.Errorf("free cost = %v", got)
	}
	if _, ok := lookup(t, reg, "fw_model_unpriced_tokens_total", free); ok {
		t.Error("a model with a zero price has unpriced tokens")
	}
	if got := value(t, reg, "fw_model_cost_usd_total", claude); !near(got, (1000*3+100*15)/1e6) {
		t.Errorf("compaction cost = %v", got)
	}
	if got := value(t, reg, "fw_model_tokens_total", map[string]string{"provider": "anthropic", "model": "claude-x", "kind": "input"}); got != 1000 {
		t.Errorf("compaction input tokens = %v", got)
	}
	// A compaction is not a model request of the run.
	if _, ok := lookup(t, reg, "fw_model_requests_total", map[string]string{"provider": "anthropic", "model": "claude-x", "outcome": "ok"}); ok {
		t.Error("a compaction counts as a model request")
	}

	// Without a price table, every token is unpriced.
	reg2 := prometheus.NewRegistry()
	g := newFeed(NewMetrics(reg2, nil), "r2", "default")
	g.emit(fw.EvRunStarted, fw.RunStartedData{})
	g.emit(fw.EvModelResponded, fw.ModelRespondedData{StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 4, OutputTokens: 2}, Provider: "anthropic", Model: "claude-x"})
	if got := value(t, reg2, "fw_model_unpriced_tokens_total", claude); got != 6 {
		t.Errorf("unpriced without prices = %v", got)
	}
}

func TestMetricsBudgetAndOrigin(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "r1", "reviewer")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "x", Origin: "a string from a caller"})
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvLoopDetected, fw.LoopDetectedData{Tool: "bash", Count: 5, Period: 1, Action: "failed"})
	f.emit(fw.EvBudgetExceeded, fw.BudgetExceededData{Limit: "maxRepeats", Value: 5})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed, Error: "budget exceeded: maxRepeats"})
	g := newFeed(m, "r2", "reviewer")
	g.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "reviewer", Input: "x", Origin: fw.OriginTool, Parent: "r1"})
	g.emit(fw.EvRunStarted, fw.RunStartedData{})
	g.emit(fw.EvBudgetExceeded, fw.BudgetExceededData{Limit: "maxCostUSD", Value: 2.5})
	g.emit(fw.EvBudgetExceeded, fw.BudgetExceededData{Limit: "a limit of a later version", Value: 1})
	g.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeFailed})
	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_runs_queued_total", map[string]string{"agent": "reviewer", "origin": "other"}, 1},
		{"fw_runs_queued_total", map[string]string{"agent": "reviewer", "origin": "tool"}, 1},
		{"fw_budget_exceeded_total", map[string]string{"agent": "reviewer", "limit": "maxRepeats"}, 1},
		{"fw_budget_exceeded_total", map[string]string{"agent": "reviewer", "limit": "maxCostUSD"}, 1},
		{"fw_budget_exceeded_total", map[string]string{"agent": "reviewer", "limit": "other"}, 1},
		{"fw_loops_total", map[string]string{"agent": "reviewer", "action": "failed"}, 1},
		{"fw_runs_total", map[string]string{"agent": "reviewer", "outcome": "failed"}, 2},
	} {
		if got := value(t, reg, c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// TestModelLabelBound sends more model names than the bound, as inline
// profiles can. Configured models keep their names, and the rest share
// "other" once the bound is reached.
func TestModelLabelBound(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	m.KnowModels("anthropic/claude-x", "", "no-provider")
	f := newFeed(m, "r1", "default")
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	call := func(model string) {
		f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "openai", Model: model})
		f.emit(fw.EvError, fw.ErrorData{Message: "model: no such model"})
	}
	call(strings.Repeat("x", maxLabelBytes+1))
	for i := 0; i < maxModelLabels+8; i++ {
		call(fmt.Sprintf("made-up-%d", i))
	}
	f.emit(fw.EvModelRequested, fw.ModelRequestedData{Provider: "anthropic", Model: "claude-x"})
	f.emit(fw.EvModelResponded, fw.ModelRespondedData{StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 1}, Provider: "anthropic", Model: "claude-x"})

	errs := map[string]string{"provider": "openai", "outcome": "error"}
	with := func(model string) map[string]string {
		out := map[string]string{"model": model}
		for k, v := range errs {
			out[k] = v
		}
		return out
	}
	if got := value(t, reg, "fw_model_requests_total", with("made-up-0")); got != 1 {
		t.Errorf("first name = %v", got)
	}
	if _, ok := lookup(t, reg, "fw_model_requests_total", with(fmt.Sprintf("made-up-%d", maxModelLabels))); ok {
		t.Error("a name past the bound is a label")
	}
	if got := value(t, reg, "fw_model_requests_total", with("other")); got != 9 {
		t.Errorf("other = %v, want 9: the long name and the 8 names past the bound", got)
	}
	if got := value(t, reg, "fw_model_requests_total", map[string]string{"provider": "anthropic", "model": "claude-x", "outcome": "ok"}); got != 1 {
		t.Errorf("a configured model lost its name: %v", got)
	}
	if n, _ := countSeries(reg, "fw_model_requests_total"); n > maxModelLabels+2 {
		t.Errorf("fw_model_requests_total has %d series", n)
	}
}

// mcpSource is a tool source with the methods of an MCP server.
type mcpSource struct {
	fw.ToolSourceFunc
	name string
	up   bool
}

func (s mcpSource) Name() string { return s.name }
func (s mcpSource) Up() bool     { return s.up }

func TestWatchMCP(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	var mu sync.Mutex
	up := true
	m.WatchMCP(func() []fw.ToolSource {
		mu.Lock()
		defer mu.Unlock()
		return []fw.ToolSource{
			mcpSource{name: "git", up: up},
			mcpSource{name: "jira", up: false},
			fw.ToolSourceFunc(func(context.Context) ([]fw.Tool, error) { return nil, nil }),
		}
	})
	if value(t, reg, "fw_mcp_server_up", map[string]string{"server": "git"}) != 1 || value(t, reg, "fw_mcp_server_up", map[string]string{"server": "jira"}) != 0 {
		t.Fatal("the scrape did not read the servers")
	}
	mu.Lock()
	up = false
	mu.Unlock()
	if value(t, reg, "fw_mcp_server_up", map[string]string{"server": "git"}) != 0 {
		t.Fatal("the gauge did not follow the server")
	}
	if n, _ := countSeries(reg, "fw_mcp_server_up"); n != 2 {
		t.Fatalf("fw_mcp_server_up has %d series", n)
	}
}

// TestMetricsLateTitle sends a title after the run finished, as a short run
// with titles: model does. The title counts its tokens and never opens the
// run again.
func TestMetricsLateTitle(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	f := newFeed(m, "r1", "default")
	f.emit(fw.EvRunQueued, fw.RunQueuedData{Agent: "default", Input: "work"})
	f.emit(fw.EvRunStarted, fw.RunStartedData{})
	f.emit(fw.EvRunFinished, fw.RunFinishedData{Outcome: fw.OutcomeCompleted})
	f.emit(fw.EvRunAnnotated, fw.RunAnnotatedData{Metadata: map[string]string{fw.MetaTitle: "Do the work"}, Usage: &fw.Usage{InputTokens: 30, OutputTokens: 4}, Provider: "local", Model: "free"})
	if got := value(t, reg, "fw_runs_active", map[string]string{"agent": "default"}); got != 0 {
		t.Errorf("fw_runs_active = %v after a late title", got)
	}
	if got := value(t, reg, "fw_model_tokens_total", map[string]string{"provider": "local", "model": "free", "kind": "input"}); got != 30 {
		t.Errorf("title input tokens = %v, want 30", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.runs) != 0 {
		t.Errorf("a late title left state for %d runs", len(m.runs))
	}
}

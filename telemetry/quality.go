package telemetry

import (
	"context"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// qualityMetrics keeps only open episodes. Receipts carry their own bounded
// cost snapshot, so late verification needs no completed-run cache.
type qualityMetrics struct {
	ledger        fw.Ledger
	runs          map[string]*qualityRun
	completions   *prometheus.CounterVec
	verifications *prometheus.CounterVec
	interventions *prometheus.CounterVec
	perRun        *prometheus.HistogramVec
	recovery      *prometheus.HistogramVec
	duplicates    prometheus.Counter
	retries       prometheus.Counter
	cost          *prometheus.HistogramVec
	costExcluded  *prometheus.CounterVec
}

type qualityRun struct {
	kept, seq             uint64
	copiedEnded           bool
	complete              bool
	origin, parent, grade string
	interventions         int
	recovering            time.Time
	last                  time.Time // last ledger evidence of activity, not restart time
	// Only outstanding requests are retained. A request for the same call id
	// after resume is ambiguous: it proves another attempt, not another effect.
	tools map[string]bool
}

func newQualityMetrics() *qualityMetrics {
	return &qualityMetrics{
		runs:          map[string]*qualityRun{},
		completions:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_task_completions_total", Help: "Own execution completion episodes, including failed and canceled, by execution and model grader judgment; no external verification is implied."}, []string{"execution", "grader_judgment"}),
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_task_verifications_total", Help: "Final trusted application receipts for completed episodes, by result; absent receipts remain unverified in the completion denominator."}, []string{"result"}),
		interventions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_operator_interventions_total", Help: "Concrete operator ledger actions: attributed decisions, user followups from interactive surfaces, and attributed interactive budget changes."}, []string{"kind"}),
		perRun:        prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fw_operator_interventions_per_run", Help: "Operator actions per own completion episode including zero, with history coverage; late actions enter the next episode.", Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21, 34}}, []string{"coverage"}),
		recovery:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fw_recovery_duration_seconds", Help: "Resume to first model response, tool response/progress, or own terminal/suspend; sleep downtime excluded. Interrupted attempts are not successes.", Buckets: []float64{0.01, 0.1, 1, 5, 15, 30, 60, 120, 300, 600}}, []string{"result"}),
		duplicates:    prometheus.NewCounter(prometheus.CounterOpts{Name: "fw_confirmed_duplicate_effects_total", Help: "Additional external effects confirmed by trusted application receipts only; retries and prevented duplicates do not count."}),
		retries:       prometheus.NewCounter(prometheus.CounterOpts{Name: "fw_ambiguous_tool_retries_total", Help: "Repeated requests of an outstanding tool call id; ambiguous attempts, never proof of duplicate external effects."}),
		cost:          prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fw_successful_task_cost_usd", Help: "Own episode model cost at final accepted or externally satisfied receipt, only with every observed token priced; child usage excluded.", Buckets: []float64{0, 0.001, 0.01, 0.1, 0.5, 1, 5, 10, 50, 100}}, []string{"basis"}),
		costExcluded:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_successful_task_cost_excluded_total", Help: "Accepted or externally satisfied receipts excluded from cost histogram because tokens lack prices."}, []string{"reason"}),
	}
}

func (q *qualityMetrics) collectors() []prometheus.Collector {
	return []prometheus.Collector{q.completions, q.verifications, q.interventions, q.perRun, q.recovery, q.duplicates, q.retries, q.cost, q.costExcluded}
}

func newQualityRun() *qualityRun { return &qualityRun{grade: "unknown", tools: map[string]bool{}} }

// seed reconstructs open episode facts without recounting historical work.
// Reading only through through makes Attach and a concurrent notification
// agree, even when the ledger already holds the next event.
func (q *qualityMetrics) seed(id string, through uint64) *qualityRun {
	st := newQualityRun()
	if q.ledger == nil {
		return st
	}
	for e, err := range q.ledger.Read(context.Background(), id, 0) {
		if err != nil || e.Seq > through {
			if err != nil {
				st.complete = false
			}
			break
		}
		q.fold(st, e, false)
		st.seq = e.Seq
	}
	return st
}

func (q *qualityMetrics) observe(e fw.Event) {
	st := q.runs[e.Subject]
	if st != nil && e.Seq != 0 && e.Seq <= st.seq {
		return
	}
	if st != nil && e.Seq != 0 && e.Seq <= st.kept {
		q.fold(st, e, false)
		st.seq = e.Seq
		if e.Seq == st.kept && st.copiedEnded {
			delete(q.runs, e.Subject)
		}
		return
	}
	// Receipts are final facts and must not create an open episode.
	if e.Type == fw.EvRunAnnotated {
		if st != nil {
			st.last = eventTime(e)
			st.seq = e.Seq
		}
		var d fw.QualityAnnotatedData
		if e.Decode(&d) == nil && d.Quality != nil {
			q.duplicates.Add(float64(d.Quality.DuplicateEffects))
			v := d.Quality.Verification
			switch v {
			case "accepted", "satisfied", "rejected", "unknown":
				q.verifications.WithLabelValues(v).Inc()
				if v == "accepted" || v == "satisfied" {
					if !d.CostComplete {
						reason := "missing_usage"
						if d.UnpricedTokens != 0 {
							reason = "unpriced_tokens"
						}
						q.costExcluded.WithLabelValues(reason).Inc()
					} else {
						q.cost.WithLabelValues(v).Observe(d.CostUSD)
					}
				}
			}
		}
		return
	}
	if st == nil {
		// Passive notes never open an episode. Count an attributed late budget
		// action now; the next episode reconstructs it from the ledger.
		if fw.LeavesFinished(e.Type) && e.Type != fw.EvRunQueued {
			if kind := intervention(e, nil); kind != "" {
				q.interventions.WithLabelValues(kind).Inc()
			}
			return
		}
		through := uint64(0)
		if e.Seq > 0 {
			through = e.Seq - 1
		}
		st = q.seed(e.Subject, through)
		q.runs[e.Subject] = st
	}
	q.fold(st, e, true)
	st.seq = e.Seq
	if e.Type == fw.EvRunFinished {
		delete(q.runs, e.Subject)
	}
}

func (q *qualityMetrics) fold(st *qualityRun, e fw.Event, count bool) {
	defer func() { st.last = eventTime(e) }()
	if e.Type == fw.EvRunQueued {
		var d fw.RunQueuedData
		_ = e.Decode(&d)
		st.kept, st.origin, st.parent = d.Kept, d.Origin, d.Parent
		st.complete = true
		st.grade = "unverified"
	}
	if e.Seq != 0 && e.Seq <= st.kept {
		switch e.Type {
		case fw.EvRunFinished:
			st.copiedEnded = true
		case fw.EvMessageReceived, fw.EvRunResumed:
			st.copiedEnded = false
		}
		return
	}
	if kind := intervention(e, st); kind != "" {
		st.interventions++
		if count {
			q.interventions.WithLabelValues(kind).Inc()
		}
	}
	if !st.recovering.IsZero() {
		result := ""
		switch e.Type {
		case fw.EvModelResponded, fw.EvToolResponded, fw.EvToolProgress:
			result = "progress"
		case fw.EvRunFinished:
			result = outcomeOf(e)
		case fw.EvRunSuspended, fw.EvRunResumed:
			result = "interrupted"
		}
		if result != "" {
			end := eventTime(e)
			// Another resume can follow a crash with no suspended event. The
			// gap is not working time. Close the incomplete old attempt at its
			// last ledger evidence, and start a fresh clock for this resume.
			if e.Type == fw.EvRunResumed {
				end = st.last
			}
			if d := end.Sub(st.recovering); count && d >= 0 {
				q.recovery.WithLabelValues(result).Observe(d.Seconds())
			}
			st.recovering = time.Time{}
		}
	}
	switch e.Type {
	case fw.EvRunResumed:
		st.recovering = eventTime(e)
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		_ = e.Decode(&d)
		if d.Origin != fw.OriginKernel {
			st.grade = "unverified"
		}
	case fw.EvOutcomeEvaluated:
		var d fw.OutcomeEvaluatedData
		_ = e.Decode(&d)
		switch d.Verdict {
		case fw.GradeSatisfied, fw.GradeNeedsRevision, fw.GradeMaxIterations, fw.GradeUnparsed, fw.GradeSkipped:
			st.grade = d.Verdict
		default:
			st.grade = "unknown"
		}
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		_ = e.Decode(&d)
		if st.tools[d.CallID] && count {
			q.retries.Inc()
		}
		st.tools[d.CallID] = true
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		_ = e.Decode(&d)
		delete(st.tools, d.CallID)
	case fw.EvRunFinished:
		if count {
			q.completions.WithLabelValues(outcomeOf(e), st.grade).Inc()
			coverage := "partial"
			if st.complete {
				coverage = "complete"
			}
			q.perRun.WithLabelValues(coverage).Observe(float64(st.interventions))
		}
		st.interventions = 0
		st.grade = "unverified"
		clear(st.tools)
	}
}

func interactive(origin string) bool {
	return origin == fw.OriginHTTP || origin == fw.OriginCLI || origin == fw.OriginMCP
}

func intervention(e fw.Event, st *qualityRun) string {
	switch e.Type {
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		_ = e.Decode(&d)
		if d.Decider != "" && d.Decider != fw.OriginKernel && st != nil && st.origin != fw.OriginKernel && d.Decider != st.parent {
			return "decision"
		}
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		_ = e.Decode(&d)
		if d.Role == fw.RoleUser && d.Note == "" && interactive(d.Origin) {
			return "followup"
		}
	case fw.EvBudgetChanged:
		var d fw.BudgetChangedData
		_ = e.Decode(&d)
		if d.By != "" && interactive(d.Origin) {
			return "budget_change"
		}
	}
	return ""
}

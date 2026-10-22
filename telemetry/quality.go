package telemetry

import (
	"context"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// qualityMetrics keeps only open episodes. Receipts carry their own bounded
// cost snapshot, so late verification needs no completed-run cache.
//
// The kernel notifies the observers inside the append of an event, so
// observe does a fixed amount of work for each event. A run that observe
// does not track and that has history, such as a follow-up of a finished
// run, gets its facts from the ledger in the background (seedLater). The
// events of the run wait in its backlog until the seed is in place.
type qualityMetrics struct {
	mu            *sync.Mutex // the mutex of Metrics; observe runs with it held
	ledger        fw.Ledger
	runs          map[string]*qualityRun
	seeds         sync.WaitGroup // the background seeds that run now
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
	// pending is the id of the approval that the run waits for, or "". A
	// run that waits for input at an interruption gets no recovery clock.
	pending    string
	recovering time.Time
	last       time.Time // last ledger evidence of activity, not restart time
	// Only outstanding requests are retained. A request for the same call id
	// after resume is ambiguous: it proves another attempt, not another effect.
	tools map[string]bool
	// seeding is true while a background read of the ledger builds the
	// facts of the run before its first observed event. The events that
	// arrive meanwhile wait in backlog, in order.
	seeding bool
	backlog []fw.Event
}

func newQualityMetrics(mu *sync.Mutex) *qualityMetrics {
	return &qualityMetrics{
		mu:            mu,
		runs:          map[string]*qualityRun{},
		completions:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_task_completions_total", Help: "Own execution completion episodes, including failed and canceled, by execution and model grader judgment; no external verification is implied."}, []string{"execution", "grader_judgment"}),
		verifications: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_task_verifications_total", Help: "Final trusted application receipts for completed episodes, by result; absent receipts remain unverified in the completion denominator."}, []string{"result"}),
		interventions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "fw_operator_interventions_total", Help: "Concrete operator ledger actions: attributed decisions, user followups from interactive surfaces, and attributed interactive budget changes."}, []string{"kind"}),
		perRun:        prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fw_operator_interventions_per_run", Help: "Operator actions per own completion episode including zero, with history coverage; late actions enter the next episode.", Buckets: []float64{0, 1, 2, 3, 5, 8, 13, 21, 34}}, []string{"coverage"}),
		recovery:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "fw_recovery_duration_seconds", Help: "Resume of a run that was queued or running to first model response, tool response/progress, approval request, or own terminal/suspend; sleep downtime excluded, runs that wait for input excluded. Interrupted attempts are not successes.", Buckets: []float64{0.01, 0.1, 1, 5, 15, 30, 60, 120, 300, 600}}, []string{"result"}),
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
// agree, even when the ledger already holds the next event. It reads the
// ledger, so it runs without the lock, in seedLater: fold with count false
// changes only the state of the run that it builds, never a metric.
func (q *qualityMetrics) seed(l fw.Ledger, id string, through uint64) *qualityRun {
	st := newQualityRun()
	if l == nil {
		return st
	}
	for e, err := range l.Read(context.Background(), id, 0) {
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

// pendingSeed is a run whose facts a background read of the ledger builds
// through the sequence number through, into the placeholder st.
type pendingSeed struct {
	id      string
	through uint64
	st      *qualityRun
}

// placeholder registers a run that waits for its seed. The caller holds
// q.mu.
func (q *qualityMetrics) placeholder(id string, through uint64) pendingSeed {
	st := &qualityRun{seeding: true}
	q.runs[id] = st
	return pendingSeed{id: id, through: through, st: st}
}

// seedLater reads the ledger for each pending seed in one goroutine, in
// order, without the lock. Then, with the lock, it puts the facts in place
// and replays the events that waited, so each event counts once and in
// order. The caller holds q.mu.
func (q *qualityMetrics) seedLater(l fw.Ledger, seeds []pendingSeed) {
	if len(seeds) == 0 {
		return
	}
	q.seeds.Add(1)
	go func() {
		defer q.seeds.Done()
		for _, p := range seeds {
			facts := q.seed(l, p.id, p.through)
			q.mu.Lock()
			backlog := p.st.backlog
			*p.st = *facts
			for _, e := range backlog {
				q.observe(e)
			}
			q.mu.Unlock()
		}
	}()
}

// wait returns when every background seed is in place. Tests use it.
func (q *qualityMetrics) wait() { q.seeds.Wait() }

// observe folds one event. The caller holds q.mu. It reads no ledger.
func (q *qualityMetrics) observe(e fw.Event) {
	st := q.runs[e.Subject]
	if st != nil && st.seeding {
		st.backlog = append(st.backlog, e)
		return
	}
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
		if e.Seq > 1 && e.Type != fw.EvRunQueued && q.ledger != nil {
			// The run has history that this process did not observe. Read
			// it in the background, and keep this event until then.
			p := q.placeholder(e.Subject, e.Seq-1)
			p.st.backlog = append(p.st.backlog, e)
			q.seedLater(q.ledger, []pendingSeed{p})
			return
		}
		st = newQualityRun()
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
	// A copied approval request counts too: a branch can begin by waiting.
	st.pending = pendingAfter(st.pending, e)
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
		case fw.EvApprovalRequested:
			// From here on the run waits for a person, and that time is not
			// recovery.
			result = "input_required"
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
		// Only a run that was queued or running at the interruption
		// recovers. A run that waits for input waits for a person, before
		// and after the restart.
		if st.pending == "" {
			st.recovering = eventTime(e)
		}
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
		// A message that a trusted caller relays from another agent is no
		// action of a person.
		fromAgent := d.From != nil && d.From.Kind == fw.FromAgent
		if d.Role == fw.RoleUser && d.Note == "" && interactive(d.Origin) && !fromAgent {
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

// noApprovalID stands for a pending approval without an id, so that the
// state of a run that waits for one is not "".
const noApprovalID = "-"

// pendingAfter returns the approval that a run waits for after e, from the
// approval that it waited for before e, as the projection of the run does:
// fw.approval.requested sets it, the decision of that approval and
// fw.run.finished clear it. "" means that the run waits for no approval.
func pendingAfter(pending string, e fw.Event) string {
	switch e.Type {
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) != nil {
			return pending
		}
		if d.ApprovalID == "" {
			return noApprovalID
		}
		return d.ApprovalID
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) != nil {
			return pending
		}
		id := d.ApprovalID
		if id == "" {
			id = noApprovalID
		}
		if id == pending {
			return ""
		}
	case fw.EvRunFinished:
		return ""
	}
	return pending
}

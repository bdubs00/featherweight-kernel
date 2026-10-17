package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// fleetScrapeTimeout bounds the work of one scrape on the fleet view. The
// first scrape starts the view, which waits up to two seconds for the
// retained records (docs/ARCHITECTURE.md section 8.6).
const fleetScrapeTimeout = 5 * time.Second

// openStates are the run states that fw_fleet_runs reports: every state
// that is not finished.
var openStates = []fw.RunState{fw.StateQueued, fw.StateRunning, fw.StateInputRequired, fw.StateSuspended}

// FleetCollector reports the fleet view of an agent at scrape time: one
// series per member and state, from the records that the view already
// holds. A scrape sends no request to another member. A member that sleeps
// under ax is therefore reported by every member that Prometheus scrapes,
// and Prometheus never wakes it.
//
// The view starts at the first scrape. The collector reads the view
// without a principal, so it counts every member and every run that the
// records hold, whatever the access rules say. The member label holds
// member names, which the deployment bounds. Without a fleet, the
// collector reports nothing.
type FleetCollector struct {
	a   *fw.Agent
	log *slog.Logger

	up       *prometheus.Desc
	lastSeen *prometheus.Desc
	runs     *prometheus.Desc
	oldest   *prometheus.Desc
}

// NewFleetCollector returns the fleet collector of a. It reads a.Fleet() at
// each scrape, so it works when the fleet starts after the registry.
func NewFleetCollector(a *fw.Agent) *FleetCollector {
	return &FleetCollector{
		a:   a,
		log: a.Logger(),
		up: prometheus.NewDesc("fw_fleet_member_up",
			"1 when the fleet member is live, else 0. Under ax, a member that is not live is usually a Task that sleeps.",
			[]string{"member"}, nil),
		lastSeen: prometheus.NewDesc("fw_fleet_member_last_seen_timestamp_seconds",
			"Unix time of the last record or heartbeat of the fleet member.",
			[]string{"member"}, nil),
		runs: prometheus.NewDesc("fw_fleet_runs",
			"Runs of the fleet member that are not finished, by state (queued, running, input-required, suspended), from its last record.",
			[]string{"member", "state"}, nil),
		oldest: prometheus.NewDesc("fw_fleet_approvals_oldest_timestamp_seconds",
			"Unix time of the request of the oldest approval that waits for a decision on the fleet member. A member without such an approval has no series.",
			[]string{"member"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *FleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.lastSeen
	ch <- c.runs
	ch <- c.oldest
}

// memberRuns is what the collector counts for one member.
type memberRuns struct {
	states map[fw.RunState]int
	oldest time.Time
}

// open counts the runs of the member that are not finished.
func (m *memberRuns) open() int {
	n := 0
	for _, c := range m.states {
		n += c
	}
	return n
}

// FleetForgetAfter is how long a member that is not live stays in the fleet
// gauges after its last record when it holds no open run. A stopped member
// without an open run leaves at once. Each rollout of a Deployment adds
// members with new pod names, so the member label stays bounded only when
// the gauges forget the members that are gone.
const FleetForgetAfter = 24 * time.Hour

func (m *memberRuns) add(r *fw.Run) {
	if r.State.Terminal() {
		return
	}
	if _, ok := m.states[r.State]; ok {
		m.states[r.State]++
	}
	if p := r.Pending; p != nil && !p.RequestedAt.IsZero() && (m.oldest.IsZero() || p.RequestedAt.Before(m.oldest)) {
		m.oldest = p.RequestedAt
	}
}

// Collect implements prometheus.Collector.
func (c *FleetCollector) Collect(ch chan<- prometheus.Metric) {
	f := c.a.Fleet()
	if f == nil {
		return
	}
	// No principal: the view counts every member and every run.
	ctx, cancel := context.WithTimeout(context.Background(), fleetScrapeTimeout)
	defer cancel()
	members, err := f.Members(ctx)
	if err != nil {
		c.log.Warn("metrics: read the fleet members", "error", err)
		return
	}
	remote, err := f.Runs(ctx)
	if err != nil {
		c.log.Warn("metrics: read the fleet runs", "error", err)
		return
	}
	local, err := c.a.List(ctx)
	if err != nil {
		c.log.Warn("metrics: list the runs", "error", err)
		return
	}
	counts := make(map[string]*memberRuns, len(members))
	for _, m := range members {
		mr := &memberRuns{states: make(map[fw.RunState]int, len(openStates))}
		for _, s := range openStates {
			mr.states[s] = 0
		}
		counts[m.Name] = mr
		if m.Self {
			for _, r := range local {
				mr.add(r)
			}
		}
	}
	for _, r := range remote {
		if mr := counts[r.Member]; mr != nil {
			mr.add(r)
		}
	}
	done := make(map[string]bool, len(members))
	for _, m := range members {
		if done[m.Name] {
			continue // a registry refuses a series twice
		}
		done[m.Name] = true
		mr := counts[m.Name]
		gone := m.State == fw.MemberStopped || (m.State != fw.MemberLive && !m.LastSeen.IsZero() && time.Since(m.LastSeen) > FleetForgetAfter)
		if gone && mr.open() == 0 {
			continue
		}
		up := 0.0
		if m.State == fw.MemberLive {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, up, m.Name)
		if !m.LastSeen.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.lastSeen, prometheus.GaugeValue, unixSeconds(m.LastSeen), m.Name)
		}
		for _, s := range openStates {
			ch <- prometheus.MustNewConstMetric(c.runs, prometheus.GaugeValue, float64(mr.states[s]), m.Name, string(s))
		}
		if !mr.oldest.IsZero() {
			ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, unixSeconds(mr.oldest), m.Name)
		}
	}
}

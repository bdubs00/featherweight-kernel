package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// fakeFleet is a fleet view with fixed members and runs. It records
// whether a call carried a principal.
type fakeFleet struct {
	mu        sync.Mutex
	members   []fw.FleetMember
	runs      []*fw.Run
	err       error
	principal bool
	forwards  int
}

func (f *fakeFleet) Name() string { return "front" }

func (f *fakeFleet) Members(ctx context.Context) ([]fw.FleetMember, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.principal = f.principal || fw.PrincipalFrom(ctx) != nil
	return f.members, f.err
}

func (f *fakeFleet) Runs(ctx context.Context) ([]*fw.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.principal = f.principal || fw.PrincipalFrom(ctx) != nil
	return f.runs, nil
}

func (f *fakeFleet) Run(context.Context, string) (*fw.Run, error) { return nil, fw.ErrRunNotFound }

func (f *fakeFleet) Forward(context.Context, string, *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwards++
	return nil, errors.New("fake: no forward")
}

func TestFleetCollector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC()
	asked := now.Add(-90 * time.Minute)
	ff := &fakeFleet{
		members: []fw.FleetMember{
			{Name: "front", Self: true, State: fw.MemberLive, LastSeen: now},
			{Name: "default/reviewer", State: fw.MemberQuiet, LastSeen: now.Add(-time.Hour)},
			{Name: "builder:8080", State: fw.MemberStopped, LastSeen: now.Add(-2 * time.Hour)},
			{Name: "default/reviewer", State: fw.MemberQuiet}, // a duplicate is reported once
			// Members that are gone and hold no open run leave the gauges.
			{Name: "fw-front-old", State: fw.MemberStopped, LastSeen: now.Add(-time.Minute)},
			{Name: "fw-front-lost", State: fw.MemberQuiet, LastSeen: now.Add(-48 * time.Hour)},
			// A member that sleeps for days with an open run stays.
			{Name: "default/sleepy", State: fw.MemberQuiet, LastSeen: now.Add(-48 * time.Hour)},
		},
		runs: []*fw.Run{
			{ID: "r1", Member: "default/reviewer", Agent: "reviewer", State: fw.StateRunning, Principal: "alice@example.com"},
			{ID: "r2", Member: "default/reviewer", Agent: "reviewer", State: fw.StateInputRequired,
				Pending: &fw.Approval{ID: "ap1", Tool: "bash", RequestedAt: asked}},
			{ID: "r3", Member: "default/reviewer", Agent: "reviewer", State: fw.StateInputRequired,
				Pending: &fw.Approval{ID: "ap2", Tool: "bash", RequestedAt: now}},
			{ID: "r4", Member: "default/reviewer", Agent: "reviewer", State: fw.StateCompleted},
			{ID: "r5", Member: "builder:8080", Agent: "builder", State: fw.StateSuspended},
			{ID: "r6", Member: "gone", Agent: "x", State: fw.StateRunning},
			{ID: "r7", Member: "fw-front-old", Agent: "x", State: fw.StateCompleted},
			{ID: "r8", Member: "default/sleepy", Agent: "x", State: fw.StateInputRequired},
		},
	}
	// The local run of this process waits for an approval.
	rm := fw.NewTool(fw.ToolSpec{Name: "rm", Destructive: true}, func(context.Context, json.RawMessage) (fw.ToolResult, error) {
		return fw.TextResult("removed"), nil
	})
	s := &script{resps: []*fw.Response{{Content: []fw.Block{toolUse("c1", "rm")}, StopReason: fw.StopToolUse}}}
	a := newAgent(t, s.model(), fw.WithTools(rm), fw.WithFleet(ff))
	r, err := a.Run(ctx, "clean up")
	if err != nil || r.State != fw.StateInputRequired {
		t.Fatalf("run = %+v, %v", r, err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewFleetCollector(a))

	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"fw_fleet_member_up", map[string]string{"member": "front"}, 1},
		{"fw_fleet_member_up", map[string]string{"member": "default/reviewer"}, 0},
		{"fw_fleet_member_up", map[string]string{"member": "builder:8080"}, 0},
		{"fw_fleet_member_last_seen_timestamp_seconds", map[string]string{"member": "default/reviewer"}, unixSeconds(now.Add(-time.Hour))},
		{"fw_fleet_runs", map[string]string{"member": "front", "state": "input-required"}, 1},
		{"fw_fleet_runs", map[string]string{"member": "front", "state": "running"}, 0},
		{"fw_fleet_runs", map[string]string{"member": "default/reviewer", "state": "running"}, 1},
		{"fw_fleet_runs", map[string]string{"member": "default/reviewer", "state": "input-required"}, 2},
		{"fw_fleet_runs", map[string]string{"member": "default/reviewer", "state": "queued"}, 0},
		{"fw_fleet_runs", map[string]string{"member": "builder:8080", "state": "suspended"}, 1},
		{"fw_fleet_approvals_oldest_timestamp_seconds", map[string]string{"member": "default/reviewer"}, unixSeconds(asked)},
		{"fw_fleet_approvals_oldest_timestamp_seconds", map[string]string{"member": "front"}, unixSeconds(r.Pending.RequestedAt)},
	} {
		if got := value(t, reg, c.name, c.labels); !near(got, c.want) {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	if n, _ := countSeries(reg, "fw_fleet_runs"); n != 4*len(openStates) {
		t.Errorf("fw_fleet_runs has %d series, want %d", n, 4*len(openStates))
	}
	for _, name := range []string{"fw-front-old", "fw-front-lost"} {
		if _, ok := lookup(t, reg, "fw_fleet_member_up", map[string]string{"member": name}); ok {
			t.Errorf("the member %s that is gone stays in the gauges", name)
		}
	}
	if got := value(t, reg, "fw_fleet_runs", map[string]string{"member": "default/sleepy", "state": "input-required"}); got != 1 {
		t.Errorf("the sleeping member with an open run = %v", got)
	}
	if _, ok := lookup(t, reg, "fw_fleet_approvals_oldest_timestamp_seconds", map[string]string{"member": "builder:8080"}); ok {
		t.Error("a member without a pending approval has an approval age")
	}
	if _, ok := lookup(t, reg, "fw_fleet_member_up", map[string]string{"member": "gone"}); ok {
		t.Error("a run of an unknown member made a member")
	}
	ff.mu.Lock()
	principal, forwards := ff.principal, ff.forwards
	ff.mu.Unlock()
	if principal || forwards != 0 {
		t.Errorf("the scrape carried a principal (%v) or forwarded %d requests", principal, forwards)
	}
	// No label carries a run id or a principal.
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		for _, mt := range mf.GetMetric() {
			for _, l := range mt.GetLabel() {
				switch l.GetValue() {
				case "r1", "r2", r.ID, "alice@example.com", "ap1":
					t.Errorf("%s has label %s=%q", mf.GetName(), l.GetName(), l.GetValue())
				}
			}
		}
	}

	// A view that fails reports nothing and does not fail the scrape.
	ff.mu.Lock()
	ff.err = errors.New("fleet: the fleet is closed")
	ff.mu.Unlock()
	if n, err := countSeries(reg, "fw_fleet_member_up"); err != nil || n != 0 {
		t.Fatalf("a failed view = %d series, %v", n, err)
	}
}

func TestFleetCollectorWithoutFleet(t *testing.T) {
	a := newAgent(t, (&script{}).model())
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewFleetCollector(a))
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 0 {
		t.Fatalf("without a fleet: %d families, %v", len(mfs), err)
	}
}

package ax

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// sweepNow is the clock of the sweep tests.
var sweepNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

// taskAt is a Task of the fake ax for a sweep test: a child of parent, or
// of nobody when parent is empty, created age before the clock, and put to
// sleep slept before the clock when its phase is Suspended. ran gives it
// the condition WorkspaceReady that ax sets at the first resume.
type taskAt struct {
	name, parent, phase string
	age, slept          time.Duration
	ran                 bool
	env                 []EnvVar
}

// add puts Tasks into the atespace team of the fake ax.
func (f *fakeAX) add(now time.Time, tasks ...taskAt) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ta := range tasks {
		spec := &v1alpha1.TaskSpec{Image: "img"}
		for _, e := range ta.env {
			spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: e.Name, Value: e.Value})
		}
		if ta.parent != "" {
			spec.Env = append(spec.Env, &v1alpha1.EnvVar{Name: EnvParentMember, Value: ta.parent})
		}
		st := &v1alpha1.TaskStatus{Phase: ta.phase}
		if ta.phase == phaseSuspended {
			st.Conditions = append(st.Conditions, &v1alpha1.Condition{Type: condReady, Status: "False", Reason: reasonSuspended, UpdateTime: timestamppb.New(now.Add(-ta.slept))})
		}
		if ta.ran {
			st.Conditions = append(st.Conditions, &v1alpha1.Condition{Type: condWorkspaceReady, Status: "True", UpdateTime: timestamppb.New(now.Add(-ta.age))})
		}
		f.tasks[key("team", ta.name)] = &v1alpha1.Task{
			Metadata: &v1alpha1.ObjectMeta{Name: ta.name, Atespace: "team", CreateTime: timestamppb.New(now.Add(-ta.age))},
			Spec:     spec,
			Status:   st,
		}
	}
}

// statusRouter plays the atenet router in front of the children for GET
// /v1/status: it answers the body of each actor, or hands the request of
// an actor to a handler, and counts the reads.
type statusRouter struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   map[string]string
	handlers map[string]http.Handler
	reads    []string
}

func newStatusRouter(t *testing.T) *statusRouter {
	t.Helper()
	r := &statusRouter{bodies: map[string]string{}, handlers: map[string]http.Handler{}}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		actor := req.Header.Get("ate-target-actor")
		r.mu.Lock()
		r.reads = append(r.reads, actor)
		body, ok := r.bodies[actor]
		h := r.handlers[actor]
		r.mu.Unlock()
		switch {
		case req.Header.Get("Authorization") != "Bearer fleet-secret":
			w.WriteHeader(http.StatusUnauthorized)
		case h != nil:
			req.Header.Del("Authorization")
			h.ServeHTTP(w, req)
		case req.Method != http.MethodGet || req.URL.Path != "/v1/status" || !ok:
			w.WriteHeader(http.StatusNotFound)
		default:
			_, _ = io.WriteString(w, body)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

// idleFor is the status of a child whose runs, total of them, open of them
// still open, last changed since before now.
func idleFor(total, open int, since time.Duration, now time.Time) string {
	return fmt.Sprintf(`{"runs":{"active":%d,"total":%d},"idle":true,"idleSince":%q}`, open, total, now.Add(-since).Format(time.RFC3339Nano))
}

func (r *statusRouter) read() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads)
}

// newSweeperAt returns the sweep of the Task team/migrator against the
// fakes, with a retention of a day, the clock at now, and a count of the
// holds of the process.
func newSweeperAt(t *testing.T, ax *fakeAX, router *statusRouter, now time.Time) (*sweeper, *int) {
	t.Helper()
	s := newSpawnerAt(t, ax, strings.TrimPrefix(router.URL, "http://"))
	s.retention = 24 * time.Hour
	s.now = func() time.Time { return now }
	holds := new(int)
	w := &sweeper{s: s, atespace: "team", member: "ax:team/migrator", hold: func() func() { *holds++; return func() { *holds-- } }, done: make(chan struct{})}
	return w, holds
}

func (f *fakeAX) gone() (deleted, suspended, resumed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleted), slices.Clone(f.suspended), slices.Clone(f.resumed)
}

// A pass removes each child Task of this Task whose runs ended a day ago,
// a child of an earlier process of the Task too, a child that never got
// its run, and a child that failed before it ran, and it sends the delete
// again for a child that ax deletes. It keeps a young child, a child whose
// runs ended lately, a child with an open run, a child that slept for less
// than the day, a blocked child, and a child that failed after it ran. It
// reads a sleeping child only after a day of sleep, holds the process
// awake for the read, and puts a child that stays back to sleep. It never
// touches a Task of another parent or a Task of a pool.
func TestSweepRemovesTheChildTasksThatNothingNeeds(t *testing.T) {
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	me := "ax:team/migrator"
	day := 24 * time.Hour
	ax.add(sweepNow,
		taskAt{name: "migrator-old", parent: me, phase: phaseSuspended, age: 3 * day, slept: 2 * day, ran: true},
		taskAt{name: "migrator-young", parent: me, phase: phaseRunning, age: 2 * time.Hour, ran: true},
		taskAt{name: "migrator-recent", parent: me, phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "migrator-waits", parent: me, phase: phaseSuspended, age: 3 * day, slept: 2 * day, ran: true},
		taskAt{name: "migrator-nap", parent: me, phase: phaseSuspended, age: 3 * day, slept: time.Hour, ran: true},
		taskAt{name: "migrator-empty", parent: me, phase: phaseRunning, age: 2 * day, ran: true},
		taskAt{name: "migrator-dead", parent: me, phase: phaseFailed, age: 2 * day},
		taskAt{name: "migrator-broke", parent: me, phase: phaseFailed, age: 2 * day, ran: true},
		taskAt{name: "migrator-blocked", parent: me, phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "migrator-going", parent: me, phase: v1alpha1.PhaseTerminating, age: time.Minute},
		taskAt{name: "other-1", parent: "ax:team/other", phase: phaseSuspended, age: 3 * day, slept: 2 * day, ran: true},
		taskAt{name: "migrator-1", phase: phaseRunning, age: 3 * day, ran: true, env: []EnvVar{{Name: "FW_POOL", Value: "migrator"}}},
	)
	router.bodies = map[string]string{
		"team/migrator-old":     idleFor(1, 0, 2*day, sweepNow),
		"team/migrator-young":   idleFor(1, 0, 2*day, sweepNow),
		"team/migrator-recent":  idleFor(2, 0, time.Hour, sweepNow),
		"team/migrator-waits":   idleFor(1, 1, 2*day, sweepNow),
		"team/migrator-nap":     idleFor(1, 0, 2*day, sweepNow),
		"team/migrator-empty":   idleFor(0, 0, 0, sweepNow),
		"team/migrator-blocked": `{"runs":{"active":1,"total":1},"idle":true,"idleSince":"2026-10-20T12:00:00Z","blocked":true}`,
		"team/other-1":          idleFor(1, 0, 2*day, sweepNow),
		"team/migrator-1":       idleFor(1, 0, 2*day, sweepNow),
	}
	w, holds := newSweeperAt(t, ax, router, sweepNow)
	held := 0
	w.hold = func() func() { held++; *holds++; return func() { *holds-- } }
	w.pass(context.Background())

	deleted, suspended, resumed := ax.gone()
	slices.Sort(deleted)
	if want := []string{"team/migrator-dead", "team/migrator-empty", "team/migrator-going", "team/migrator-old"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
	if want := []string{"team/migrator-waits"}; !slices.Equal(suspended, want) {
		t.Fatalf("suspended = %v, want %v", suspended, want)
	}
	slices.Sort(resumed)
	// One hold covers the wakes of the pass, and the pass releases it.
	if want := []string{"team/migrator-old", "team/migrator-waits"}; !slices.Equal(resumed, want) || held != 1 || *holds != 0 {
		t.Fatalf("resumed = %v, want %v; holds taken %d, open %d", resumed, want, held, *holds)
	}
	reads := router.read()
	slices.Sort(reads)
	if want := []string{"team/migrator-blocked", "team/migrator-empty", "team/migrator-old", "team/migrator-recent", "team/migrator-waits"}; !slices.Equal(reads, want) {
		t.Fatalf("status reads = %v, want %v", reads, want)
	}
}

// A child Task goes with the Tasks under it, the lowest first, and only
// when every one of them is due: an open run of a grandchild keeps its
// parent too, and the read stops there.
func TestSweepRemovesATreeFromTheBottom(t *testing.T) {
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	day := 24 * time.Hour
	ax.add(sweepNow,
		taskAt{name: "kid", parent: "ax:team/migrator", phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "kid-grand", parent: "ax:team/kid", phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "kid-grand-great", parent: "ax:team/kid-grand", phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "busy", parent: "ax:team/migrator", phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "busy-grand", parent: "ax:team/busy", phase: phaseRunning, age: 3 * day, ran: true},
		taskAt{name: "busy-grand-great", parent: "ax:team/busy-grand", phase: phaseRunning, age: 3 * day, ran: true},
	)
	for _, n := range []string{"kid", "kid-grand", "kid-grand-great", "busy", "busy-grand-great"} {
		router.bodies["team/"+n] = idleFor(1, 0, 2*day, sweepNow)
	}
	router.bodies["team/busy-grand"] = `{"runs":{"active":1,"total":1},"idle":false}`
	w, _ := newSweeperAt(t, ax, router, sweepNow)
	w.pass(context.Background())

	deleted, _, _ := ax.gone()
	if want := []string{"team/kid-grand-great", "team/kid-grand", "team/kid"}; !slices.Equal(deleted, want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
	if reads := router.read(); slices.Contains(reads, "team/busy-grand-great") || !slices.Contains(reads, "team/busy-grand") {
		t.Fatalf("status reads = %v", reads)
	}
}

// The sweep keeps no state of its own: the clocks are the creation and the
// sleep of each Task on the ax API and the newest update of a run in the
// ledger of the child. So a process that starts again, here a new spawner
// two hours later, removes the children that the earlier process kept
// because their day had not passed, a child that slept the whole time too.
func TestSweepAfterARestartOfTheParent(t *testing.T) {
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	day := 24 * time.Hour
	ax.add(sweepNow,
		taskAt{name: "migrator-late", parent: "ax:team/migrator", phase: phaseRunning, age: 2 * day, ran: true},
		taskAt{name: "migrator-sleeper", parent: "ax:team/migrator", phase: phaseSuspended, age: 2 * day, slept: 23 * time.Hour, ran: true},
	)
	router.bodies["team/migrator-late"] = idleFor(1, 0, 23*time.Hour, sweepNow)
	router.bodies["team/migrator-sleeper"] = idleFor(1, 0, 23*time.Hour+10*time.Minute, sweepNow)
	first, _ := newSweeperAt(t, ax, router, sweepNow)
	first.pass(context.Background())
	if deleted, _, resumed := ax.gone(); len(deleted) != 0 || len(resumed) != 0 {
		t.Fatalf("the first process deleted %v and woke %v", deleted, resumed)
	}
	_ = first.s.Close()

	later, _ := newSweeperAt(t, ax, router, sweepNow.Add(2*time.Hour))
	later.pass(context.Background())
	deleted, _, resumed := ax.gone()
	slices.Sort(deleted)
	if want := []string{"team/migrator-late", "team/migrator-sleeper"}; !slices.Equal(deleted, want) || !slices.Equal(resumed, []string{"team/migrator-sleeper"}) {
		t.Fatalf("after the restart: deleted %v, want %v; woke %v", deleted, want, resumed)
	}
}

// One pass wakes at most sweepWakes sleeping children for a read, so a
// pass stays short; the next pass takes the rest.
func TestSweepWakesAFewChildTasksPerPass(t *testing.T) {
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	day := 24 * time.Hour
	for i := range sweepWakes + 2 {
		name := fmt.Sprintf("migrator-%02d", i+10)
		ax.add(sweepNow, taskAt{name: name, parent: "ax:team/migrator", phase: phaseSuspended, age: 3 * day, slept: 2 * day, ran: true})
		router.bodies["team/"+name] = idleFor(1, 0, 2*day, sweepNow)
	}
	w, _ := newSweeperAt(t, ax, router, sweepNow)
	w.pass(context.Background())
	if deleted, _, _ := ax.gone(); len(deleted) != sweepWakes {
		t.Fatalf("the first pass deleted %d, want %d", len(deleted), sweepWakes)
	}
	w.pass(context.Background())
	if deleted, _, _ := ax.gone(); len(deleted) != sweepWakes+2 {
		t.Fatalf("two passes deleted %d, want %d", len(deleted), sweepWakes+2)
	}
}

// The sweep reads the status that a real child serves: a child whose run
// finished stays for the retention and goes after it.
func TestSweepReadsTheStatusOfARealChild(t *testing.T) {
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	child, _ := childProcessAgent(t)
	h, err := child.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if r, err := child.Run(context.Background(), "migrate module x"); err != nil || r.State != fw.StateCompleted {
		t.Fatalf("Run = %+v, %v", r, err)
	}
	router.handlers["team/migrator-real"] = h
	now := time.Now()
	ax.add(now, taskAt{name: "migrator-real", parent: "ax:team/migrator", phase: phaseRunning, age: 3 * 24 * time.Hour, ran: true})

	soon, _ := newSweeperAt(t, ax, router, now.Add(time.Hour))
	soon.pass(context.Background())
	if deleted, _, _ := ax.gone(); len(deleted) != 0 {
		t.Fatalf("deleted %v an hour after the run", deleted)
	}
	later, _ := newSweeperAt(t, ax, router, now.Add(25*time.Hour))
	later.pass(context.Background())
	if deleted, _, _ := ax.gone(); !slices.Equal(deleted, []string{"team/migrator-real"}) {
		t.Fatalf("deleted %v a day after the run", deleted)
	}
}

// The service sweeps in a process of a Task with the ax driver that can
// start children, and stops with the agent. A process outside a Task, a
// child that cannot start children, and a retention below zero have no
// sweep.
func TestSweepRunsWhereChildrenCanBe(t *testing.T) {
	prevFirst := sweepFirst
	sweepFirst = time.Millisecond
	t.Cleanup(func() { sweepFirst = prevFirst })
	ax := newFakeAX(t)
	router := newStatusRouter(t)
	newSpawnerAt(t, ax, strings.TrimPrefix(router.URL, "http://")) // stashes team/migrator
	agent := func(edit func(*fw.Config)) *fw.Agent {
		cfg := fw.DefaultConfig()
		cfg.Spawn.Driver = "ax"
		cfg.AX.Server = ax.addr
		cfg.AX.Router = strings.TrimPrefix(router.URL, "http://")
		edit(cfg)
		a, err := fw.New(fw.WithConfig(cfg), fw.WithoutServices(), fw.WithAuthn(fw.NoAuthn{}), fw.WithLedger(fw.NewMemLedger()),
			fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
		if err != nil {
			t.Fatalf("fw.New: %v", err)
		}
		t.Cleanup(func() { a.Close(context.Background()) })
		return a
	}
	c, err := startSweep(context.Background(), agent(func(*fw.Config) {}))
	if err != nil || c == nil {
		t.Fatalf("startSweep = %v, %v", c, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ax.mu.Lock()
		lists := ax.lists
		ax.mu.Unlock()
		if lists > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweep listed no Tasks")
		}
		time.Sleep(time.Millisecond)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*fw.Config){
		"spawn disabled":  func(c *fw.Config) { c.Spawn.Disabled = true },
		"retention off":   func(c *fw.Config) { c.AX.ChildRetention = -1 },
		"no spawn driver": func(c *fw.Config) { c.Spawn.Driver = "" },
	} {
		if c, err := startSweep(context.Background(), agent(edit)); c != nil || err != nil {
			t.Fatalf("%s: startSweep = %v, %v", name, c, err)
		}
	}
	prevTask, prevWS := Stashed()
	Stash(nil, nil)
	defer Stash(prevTask, prevWS)
	if c, err := startSweep(context.Background(), agent(func(*fw.Config) {})); c != nil || err != nil {
		t.Fatalf("outside a Task: startSweep = %v, %v", c, err)
	}
}

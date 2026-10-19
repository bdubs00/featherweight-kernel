package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// crashSpawner plays a platform of child Tasks. Each child runs in another
// agent, as in a process of its own, and the spawner keeps the processes
// that it created by member, so a test sees every child Task. A test stops
// Spawn at a boundary, as a parent process that dies there would leave it.
type crashSpawner struct {
	other *Agent

	mu        sync.Mutex
	stopAt    string          // the boundary where the next Spawn stops
	reached   chan struct{}   // closed when Spawn reached stopAt
	hold      chan struct{}   // a stopped Spawn waits for it
	goOn      bool            // the stopped Spawn goes on after the hold, instead of failing
	tasks     map[string]bool // the processes that exist, by member
	created   []string        // every process that Spawn created, in order
	spawns    int             // the calls of Spawn
	abandoned []string        // the members that Abandon removed
	findErr   error           // Find fails with it, as a platform that does not answer

	abandons     int           // the calls of Abandon
	abandonFails int           // that many calls of Abandon fail first
	abandonGate  chan struct{} // when set, Abandon waits for it
	abandonAt    chan struct{} // closed when an Abandon waits at the gate
}

// The boundaries of a start where a test stops Spawn.
const (
	beforeTask = "before the Task exists"
	beforeRun  = "after the Task exists, before the child run started"
	beforeEnd  = "after the child run started, before Spawn returns"
)

func newCrashSpawner(other *Agent) *crashSpawner {
	return &crashSpawner{other: other, tasks: map[string]bool{}}
}

// stop makes the next Spawn stop at the boundary at. It returns a channel
// that closes when Spawn gets there, and a function that lets the stopped
// Spawn return an error, as the call of a process that is gone.
func (s *crashSpawner) stop(at string) (<-chan struct{}, func()) {
	return s.stopThen(at, false)
}

// pauseAt is stop for a Spawn that goes on after the release.
func (s *crashSpawner) pauseAt(at string) (<-chan struct{}, func()) {
	return s.stopThen(at, true)
}

func (s *crashSpawner) stopThen(at string, goOn bool) (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopAt, s.reached, s.hold, s.goOn = at, make(chan struct{}), make(chan struct{}), goOn
	hold := s.hold
	var once sync.Once
	return s.reached, func() { once.Do(func() { close(hold) }) }
}

// pause reports whether Spawn stopped at the boundary at. It returns when
// the test lets the stopped Spawn go or when ctx ends.
func (s *crashSpawner) pause(ctx context.Context, at string) bool {
	s.mu.Lock()
	stop, goOn := s.stopAt == at, s.goOn
	hold := s.hold
	if stop {
		s.stopAt = ""
		close(s.reached)
	}
	s.mu.Unlock()
	if stop {
		select {
		case <-hold:
		case <-ctx.Done():
			goOn = false
		}
	}
	return stop && !goOn
}

// taskOf is the name of the process of a child: a function of the id of
// the child, as the ax driver makes it.
func taskOf(id string) string { return "task-" + strings.ToLower(id) }

func (s *crashSpawner) Plan(_ *Run, req RunRequest) (string, error) { return taskOf(req.ID), nil }

var errGone = errors.New("the process is gone")

func (s *crashSpawner) Spawn(ctx context.Context, _ *Run, req RunRequest) (*Run, string, error) {
	s.mu.Lock()
	s.spawns++
	s.mu.Unlock()
	if req.ID == "" {
		req.ID = NewID() // a kernel that names no child
	}
	member := taskOf(req.ID)
	if s.pause(ctx, beforeTask) {
		return nil, "", errGone
	}
	s.mu.Lock()
	s.tasks[member] = true
	s.created = append(s.created, member)
	s.mu.Unlock()
	if s.pause(ctx, beforeRun) {
		// A start that ends here leaves its process, as the ax driver can
		// when a create ends on a cut call: the kernel abandons it.
		return nil, "", errGone
	}
	r, err := s.other.Start(ctx, req.Input, WithRequest(req))
	if err != nil {
		s.mu.Lock()
		delete(s.tasks, member)
		s.mu.Unlock()
		return nil, "", err
	}
	if s.pause(ctx, beforeEnd) {
		return nil, "", errGone
	}
	return r, member, nil
}

// alive returns ErrRunNotFound for a member that has no process.
func (s *crashSpawner) alive(member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tasks[member] {
		return fmt.Errorf("%w: no process %s", ErrRunNotFound, member)
	}
	return nil
}

func (s *crashSpawner) Find(ctx context.Context, member, id string) (*Run, error) {
	s.mu.Lock()
	err := s.findErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := s.alive(member); err != nil {
		return nil, err
	}
	return s.other.Get(ctx, id)
}

func (s *crashSpawner) Get(ctx context.Context, member, id string) (*Run, error) {
	if err := s.alive(member); err != nil {
		return nil, err
	}
	return s.other.Get(ctx, id)
}

func (s *crashSpawner) WaitFor(ctx context.Context, member, id string, done func(*Run) bool) (*Run, error) {
	if err := s.alive(member); err != nil {
		return nil, err
	}
	return s.other.WaitFor(ctx, id, done)
}

func (s *crashSpawner) Decide(ctx context.Context, member, id string, d ApprovalDecision) error {
	if err := s.alive(member); err != nil {
		return err
	}
	return s.other.Decide(ctx, id, d)
}

func (s *crashSpawner) Cancel(ctx context.Context, member, id string) error {
	if err := s.alive(member); err != nil {
		return err
	}
	return s.other.Cancel(ctx, id)
}

func (s *crashSpawner) Abandon(ctx context.Context, member string) error {
	s.mu.Lock()
	s.abandons++
	gate, at := s.abandonGate, s.abandonAt
	s.abandonGate = nil
	fail := s.abandonFails > 0
	if fail {
		s.abandonFails--
	}
	s.mu.Unlock()
	if gate != nil {
		close(at)
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fail {
		return errors.New("the platform refused the delete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, member)
	s.abandoned = append(s.abandoned, member)
	return nil
}

func (s *crashSpawner) setFindErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.findErr = err
}

// counts returns the calls of Spawn, the processes created, and the
// processes abandoned.
func (s *crashSpawner) counts() (spawns int, created, abandoned []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns, append([]string(nil), s.created...), append([]string(nil), s.abandoned...)
}

// deadSpawner is the spawner of a process that can die: after the cut,
// nothing that the process asks of the platform has an effect, and the
// call that was in flight ends with an error.
type deadSpawner struct {
	*crashSpawner
	cut atomic.Bool
}

func (d *deadSpawner) gone() error {
	if d.cut.Load() {
		return errGone
	}
	return nil
}

func (d *deadSpawner) Find(ctx context.Context, member, id string) (*Run, error) {
	if err := d.gone(); err != nil {
		return nil, err
	}
	return d.crashSpawner.Find(ctx, member, id)
}

func (d *deadSpawner) Get(ctx context.Context, member, id string) (*Run, error) {
	if err := d.gone(); err != nil {
		return nil, err
	}
	return d.crashSpawner.Get(ctx, member, id)
}

func (d *deadSpawner) Cancel(ctx context.Context, member, id string) error {
	if err := d.gone(); err != nil {
		return err
	}
	return d.crashSpawner.Cancel(ctx, member, id)
}

func (d *deadSpawner) Abandon(ctx context.Context, member string) error {
	if err := d.gone(); err != nil {
		return err
	}
	return d.crashSpawner.Abandon(ctx, member)
}

// cutLedger is the ledger of a process that can die: after the cut, no
// event reaches the disk.
type cutLedger struct {
	Ledger
	cut atomic.Bool
}

func (l *cutLedger) Append(ctx context.Context, e Event) (uint64, error) {
	if l.cut.Load() {
		return 0, errGone
	}
	return l.Ledger.Append(ctx, e)
}

// spawnTool starts a child of its run as spawn_task does, keyed by its
// tool call, and waits for the child to end.
func spawnTool() Tool {
	spec := ToolSpec{Name: "spawn", Description: "start a child", InputSchema: json.RawMessage(`{"type":"object"}`)}
	return NewTool(spec, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		a, info := AgentFrom(ctx), RunInfoFrom(ctx)
		child, err := a.SpawnChild(ctx, info.RunID, NewRunRequest("child work", WithIdempotencyKey("spawn:"+info.RunID+":"+info.CallID)))
		if err != nil {
			return ErrorResult("spawn: " + err.Error()), nil
		}
		done, err := a.WaitFor(ctx, child.ID, func(r *Run) bool { return r.State.Terminal() })
		if err != nil {
			return ErrorResult("spawn: wait: " + err.Error()), nil
		}
		return TextResult(fmt.Sprintf("child %s %s: %s", done.ID, done.State, done.Output)), nil
	})
}

func openFileLedgerAt(t *testing.T, root string) *FileLedger {
	t.Helper()
	l, err := OpenFileLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// recordedChildren returns the children that the ledger of a run records,
// with their members, in the order of the first record of each.
func recordedChildren(t *testing.T, l Ledger, run string) (ids []string, members map[string]string) {
	t.Helper()
	members = map[string]string{}
	for _, e := range ledgerEvents(t, l, run) {
		var d RunChildData
		if e.Type != EvRunChild || e.Decode(&d) != nil || d.Child == "" {
			continue
		}
		if _, ok := members[d.Child]; !ok {
			ids = append(ids, d.Child)
		}
		if d.Member != "" {
			members[d.Child] = d.Member
		}
	}
	return ids, members
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// crashDuringSpawn starts a parent run whose spawn tool stops at the
// boundary at, kills the parent process there, and opens the ledger in a
// new process with the options. It returns the new process, the parent
// run, and the ledger of the new process.
func crashDuringSpawn(t *testing.T, sp *crashSpawner, at string, opts ...Option) (*Agent, string, Ledger) {
	t.Helper()
	ctx := testCtx(t)
	root := filepath.Join(t.TempDir(), "runs")
	cut := &cutLedger{Ledger: openFileLedgerAt(t, root)}
	dead := &deadSpawner{crashSpawner: sp}
	reached, release := sp.stop(at)
	a := newTestAgent(t, newScript(callTools(use("c1", "spawn", `{}`))).Model(), WithLedger(cut), WithSpawner(dead), WithTools(spawnTool()))
	parent, err := a.Start(ctx, "parent work")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatalf("Spawn never got to the boundary %q", at)
	}
	// The parent process dies here: nothing it does from now on reaches
	// the disk or the platform, and its spawner call ends.
	cut.cut.Store(true)
	dead.cut.Store(true)
	release()

	l := openFileLedgerAt(t, root)
	b := newTestAgent(t, newScript(endTurn("parent done")).Model(), append([]Option{WithLedger(l), WithSpawner(sp), WithTools(spawnTool())}, opts...)...)
	return b, parent.ID, l
}

// approveRerun approves the rerun of the open spawn call of the parent
// after the restart.
func approveRerun(t *testing.T, b *Agent, parent string) {
	t.Helper()
	ctx := testCtx(t)
	var pending *Approval
	eventually(t, "the rerun after resume waits for a decision", func() bool {
		r, err := b.Get(ctx, parent)
		if err == nil && r.Pending != nil {
			pending = r.Pending
		}
		return pending != nil
	})
	if pending.Reason != "rerun after resume" || pending.CallID != "c1" {
		t.Fatalf("pending = %+v", pending)
	}
	if err := b.Decide(ctx, parent, ApprovalDecision{ApprovalID: pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
}

// assertTracked fails for a process that the spawner created and that the
// parent neither follows as a child nor abandoned.
func assertTracked(t *testing.T, b *Agent, sp *crashSpawner, parent string) {
	t.Helper()
	ctx := testCtx(t)
	kids, err := b.Children(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	followed := map[string]bool{}
	for _, k := range kids {
		followed[k.Member] = true
	}
	_, created, abandoned := sp.counts()
	gone := map[string]bool{}
	for _, m := range abandoned {
		gone[m] = true
	}
	for _, m := range created {
		if !followed[m] && !gone[m] {
			t.Fatalf("the process %s runs, and the parent neither follows nor abandoned it (children %+v, abandoned %v)", m, kids, abandoned)
		}
	}
}

// A crash of the parent process while a child of spawn_task starts leaves
// the parent knowing the child, at every boundary of the start. A child
// that never got its run is recorded as failed, and its process is
// abandoned; the approved rerun of the call then starts the only child of
// the call. A child that got its run is found and followed, and the rerun
// takes it again instead of starting a second one.
func TestSpawnSurvivesACrashAtEveryBoundary(t *testing.T) {
	for _, at := range []string{beforeTask, beforeRun} {
		t.Run(at, func(t *testing.T) {
			ctx := testCtx(t)
			other := newTestAgent(t, newScript(endTurn("child done")).Model())
			sp := newCrashSpawner(other)
			b, parent, l := crashDuringSpawn(t, sp, at)

			// The new process knows the child from the record before its
			// start, and records it as lost.
			ids, members := recordedChildren(t, l, parent)
			if len(ids) != 1 || members[ids[0]] == "" {
				t.Fatalf("the parent records the children %v at %v", ids, members)
			}
			lost, member := ids[0], members[ids[0]]
			eventually(t, "the lost child is recorded as failed", func() bool {
				r, err := b.Get(ctx, lost)
				return err == nil && r.State == StateFailed
			})
			r, _ := b.Get(ctx, lost)
			if !strings.Contains(r.Error, "restarted before the child run started in "+member) {
				t.Fatalf("the lost child = %+v", r)
			}
			eventually(t, "the process of the lost child is abandoned", func() bool {
				_, _, abandoned := sp.counts()
				return len(abandoned) == 1 && abandoned[0] == member
			})

			// The rerun of the call starts the one child of the call.
			approveRerun(t, b, parent)
			done, err := b.WaitFor(ctx, parent, func(r *Run) bool { return r.State.Terminal() })
			mustState(t, done, err, StateCompleted)
			list, _ := other.List(ctx)
			if len(list) != 1 || list[0].Output != "child done" || list[0].ID == lost {
				t.Fatalf("the child process holds %+v", list)
			}
			// The parent lists the lost child as failed, beside the child of
			// the rerun.
			kids, err := b.Children(ctx, parent)
			states := map[string]RunState{}
			for _, k := range kids {
				states[k.ID] = k.State
			}
			if err != nil || len(kids) != 2 || states[list[0].ID] != StateCompleted || states[lost] != StateFailed {
				t.Fatalf("children of the parent = %v, %v", states, err)
			}
			if spawns, _, _ := sp.counts(); spawns != 2 {
				t.Fatalf("Spawn ran %d times, want one per attempt", spawns)
			}
			assertTracked(t, b, sp, parent)
		})
	}

	t.Run(beforeEnd, func(t *testing.T) {
		ctx := testCtx(t)
		other := newTestAgent(t, blockingModel(nil))
		sp := newCrashSpawner(other)
		b, parent, l := crashDuringSpawn(t, sp, beforeEnd)

		ids, members := recordedChildren(t, l, parent)
		if len(ids) != 1 || members[ids[0]] == "" {
			t.Fatalf("the parent records the children %v at %v", ids, members)
		}
		child, member := ids[0], members[ids[0]]
		// The new process finds the child in its process and follows it:
		// Get, WaitFor, and Children reach it there.
		eventually(t, "the child is found in its process", func() bool {
			r, err := b.Get(ctx, child)
			return err == nil && r.State == StateRunning && r.Member == member
		})
		r, err := b.WaitFor(ctx, child, func(r *Run) bool { return r.State == StateRunning })
		if err != nil || r.Member != member || r.Parent != parent {
			t.Fatalf("WaitFor the found child = %+v, %v", r, err)
		}
		assertTracked(t, b, sp, parent)

		// The rerun of the call takes the child again and waits for it.
		approveRerun(t, b, parent)
		eventually(t, "the rerun waits for the child", func() bool {
			r, _ := b.Get(ctx, parent)
			return r != nil && r.State == StateRunning && r.Pending == nil
		})
		if err := b.Cancel(ctx, child); err != nil {
			t.Fatalf("Cancel the found child: %v", err)
		}
		done, err := b.WaitFor(ctx, parent, func(r *Run) bool { return r.State.Terminal() })
		mustState(t, done, err, StateCompleted)
		list, _ := other.List(ctx)
		if len(list) != 1 || list[0].ID != child || list[0].State != StateCanceled {
			t.Fatalf("the child process holds %+v", list)
		}
		if spawns, created, abandoned := sp.counts(); spawns != 1 || len(created) != 1 || len(abandoned) != 0 {
			t.Fatalf("Spawn ran %d times, created %v, abandoned %v", spawns, created, abandoned)
		}
		assertTracked(t, b, sp, parent)
	})
}

// A restart while the spawner cannot tell whether the child exists keeps
// the child: the parent answers for it as queued, removes nothing, and
// looks again, and the rerun of the call waits instead of starting a
// second child. Once the spawner answers, the parent follows the child.
func TestSpawnLookGoesOnWhileTheSpawnerCannotTell(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, blockingModel(nil))
	sp := newCrashSpawner(other)
	sp.setFindErr(errors.New("the platform does not answer"))
	b, parent, l := crashDuringSpawn(t, sp, beforeEnd)

	ids, members := recordedChildren(t, l, parent)
	if len(ids) != 1 {
		t.Fatalf("the parent records the children %v", ids)
	}
	child, member := ids[0], members[ids[0]]
	approveRerun(t, b, parent)
	time.Sleep(300 * time.Millisecond)
	r, err := b.Get(ctx, child)
	if err != nil || r.State != StateQueued || r.Member != member || r.Parent != parent {
		t.Fatalf("the child while the spawner cannot tell = %+v, %v", r, err)
	}
	// The look goes on at the next start, so it does not keep the process
	// awake.
	if b.Starting() != 0 {
		t.Fatalf("Starting = %d while the parent looks for its child", b.Starting())
	}
	if spawns, _, abandoned := sp.counts(); spawns != 1 || len(abandoned) != 0 {
		t.Fatalf("Spawn ran %d times and abandoned %v while the spawner could not tell", spawns, abandoned)
	}

	sp.setFindErr(nil)
	eventually(t, "the child is found once the spawner answers", func() bool {
		r, err := b.Get(ctx, child)
		return err == nil && r.State == StateRunning
	})
	if err := b.Cancel(ctx, child); err != nil {
		t.Fatal(err)
	}
	done, err := b.WaitFor(ctx, parent, func(r *Run) bool { return r.State.Terminal() })
	mustState(t, done, err, StateCompleted)
	if spawns, created, abandoned := sp.counts(); spawns != 1 || len(created) != 1 || len(abandoned) != 0 {
		t.Fatalf("Spawn ran %d times, created %v, abandoned %v", spawns, created, abandoned)
	}
	if list, _ := other.List(ctx); len(list) != 1 || list[0].State != StateCanceled {
		t.Fatalf("the child process holds %+v", list)
	}
}

// plainSpawner is a spawner that names no process before it starts a
// child, so a restart cannot look for the child.
type plainSpawner struct{ s *crashSpawner }

func (p plainSpawner) Spawn(ctx context.Context, parent *Run, req RunRequest) (*Run, string, error) {
	return p.s.Spawn(ctx, parent, req)
}
func (p plainSpawner) Get(ctx context.Context, member, id string) (*Run, error) {
	return p.s.Get(ctx, member, id)
}
func (p plainSpawner) WaitFor(ctx context.Context, member, id string, done func(*Run) bool) (*Run, error) {
	return p.s.WaitFor(ctx, member, id, done)
}
func (p plainSpawner) Decide(ctx context.Context, member, id string, d ApprovalDecision) error {
	return p.s.Decide(ctx, member, id, d)
}
func (p plainSpawner) Cancel(ctx context.Context, member, id string) error {
	return p.s.Cancel(ctx, member, id)
}

// A spawner that names no process up front still gets the record of the
// child before its start. After a crash, the parent cannot look for the
// child, so it records the child as failed with the reason, and does not
// take it for a child that it follows.
func TestSpawnWithASpawnerThatNamesNoProcess(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript().Model())
	cs := newCrashSpawner(other)
	root := filepath.Join(t.TempDir(), "runs")
	cut := &cutLedger{Ledger: openFileLedgerAt(t, root)}
	reached, release := cs.stop(beforeRun)
	a := newTestAgent(t, newScript(callTools(use("c1", "spawn", `{}`))).Model(), WithLedger(cut), WithSpawner(plainSpawner{cs}), WithTools(spawnTool()))
	parent, err := a.Start(ctx, "parent work")
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	cut.cut.Store(true)
	release()

	l := openFileLedgerAt(t, root)
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithSpawner(plainSpawner{cs}), WithTools(spawnTool()))
	ids, _ := recordedChildren(t, l, parent.ID)
	if len(ids) != 1 {
		t.Fatalf("the parent records the children %v", ids)
	}
	r, err := b.Get(ctx, ids[0])
	if err != nil || r.State != StateFailed || !strings.Contains(r.Error, "cannot be looked for") {
		t.Fatalf("the child after the restart = %+v, %v", r, err)
	}
}

package builtin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// taskSpawner plays a platform of child Tasks for spawn_task. Each child
// runs in another agent under the id that the parent chose, and the
// spawner keeps the processes that it created by member. A test stops one
// Spawn after the child run started and before Spawn returns, as a parent
// process that dies there would leave it.
type taskSpawner struct {
	other *fw.Agent

	mu        sync.Mutex
	stop      bool          // the next Spawn stops after the child run started
	stopped   chan struct{} // closed when that Spawn got there
	hold      chan struct{} // the stopped Spawn waits for it
	tasks     map[string]bool
	spawns    int
	abandoned []string
}

func newTaskSpawner(other *fw.Agent) *taskSpawner {
	return &taskSpawner{other: other, tasks: map[string]bool{}}
}

// stopAfterStart makes the next Spawn stop after the child run started. It
// returns a channel that closes when Spawn gets there, and a function that
// lets that Spawn return an error, as the call of a process that is gone.
func (s *taskSpawner) stopAfterStart() (<-chan struct{}, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop, s.stopped, s.hold = true, make(chan struct{}), make(chan struct{})
	hold := s.hold
	var once sync.Once
	return s.stopped, func() { once.Do(func() { close(hold) }) }
}

func (s *taskSpawner) Plan(_ *fw.Run, req fw.RunRequest) (string, error) {
	return "task-" + strings.ToLower(req.ID), nil
}

func (s *taskSpawner) Spawn(ctx context.Context, _ *fw.Run, req fw.RunRequest) (*fw.Run, string, error) {
	if req.ID == "" {
		req.ID = fw.NewID() // a kernel that names no child
	}
	member := "task-" + strings.ToLower(req.ID)
	s.mu.Lock()
	s.spawns++
	s.tasks[member] = true
	s.mu.Unlock()
	r, err := s.other.Start(ctx, req.Input, fw.WithRequest(req))
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	stop, hold := s.stop, s.hold
	if stop {
		s.stop = false
		close(s.stopped)
	}
	s.mu.Unlock()
	if stop {
		<-hold
		return nil, "", errors.New("the process is gone")
	}
	return r, member, nil
}

func (s *taskSpawner) alive(member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tasks[member] {
		return fmt.Errorf("%w: no process %s", fw.ErrRunNotFound, member)
	}
	return nil
}

func (s *taskSpawner) Find(ctx context.Context, member, id string) (*fw.Run, error) {
	return s.Get(ctx, member, id)
}

func (s *taskSpawner) Get(ctx context.Context, member, id string) (*fw.Run, error) {
	if err := s.alive(member); err != nil {
		return nil, err
	}
	return s.other.Get(ctx, id)
}

func (s *taskSpawner) WaitFor(ctx context.Context, member, id string, done func(*fw.Run) bool) (*fw.Run, error) {
	if err := s.alive(member); err != nil {
		return nil, err
	}
	return s.other.WaitFor(ctx, id, done)
}

func (s *taskSpawner) Decide(ctx context.Context, member, id string, d fw.ApprovalDecision) error {
	if err := s.alive(member); err != nil {
		return err
	}
	return s.other.Decide(ctx, id, d)
}

func (s *taskSpawner) Cancel(ctx context.Context, member, id string) error {
	if err := s.alive(member); err != nil {
		return err
	}
	return s.other.Cancel(ctx, id)
}

func (s *taskSpawner) Abandon(_ context.Context, member string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, member)
	s.abandoned = append(s.abandoned, member)
	return nil
}

// cutLedger is the ledger of a process that can die: after the cut, no
// event reaches the disk.
type cutLedger struct {
	fw.Ledger
	cut atomic.Bool
}

func (l *cutLedger) Append(ctx context.Context, e fw.Event) (uint64, error) {
	if l.cut.Load() {
		return 0, errors.New("the process is gone")
	}
	return l.Ledger.Append(ctx, e)
}

func openRunsAt(t *testing.T, root string) *fw.FileLedger {
	t.Helper()
	l, err := fw.OpenFileLedger(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// approveRerunOf approves the rerun of the open call c1 of a run after a
// restart.
func approveRerunOf(t *testing.T, ctx context.Context, a *fw.Agent, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, err := a.Get(ctx, id)
		if err == nil && r.Pending != nil {
			if r.Pending.Reason != "rerun after resume" || r.Pending.CallID != "c1" {
				t.Fatalf("pending = %+v", r.Pending)
			}
			if err := a.Decide(ctx, id, fw.ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never asked to rerun its call: %+v, %v", id, r, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// parentGot answers the parent after the tool result with the text of the
// result, so the output of the parent shows what spawn_task returned.
func parentGot(t *testing.T) func(*fw.Request) *fw.Response {
	return func(req *fw.Request) *fw.Response {
		return endTurn("parent got: " + fw.TextOf(lastToolResults(t, req)[0].Content))
	}
}

// A parent process that dies after its child started in another process,
// before it recorded the start, leaves the call of spawn_task open. After
// the restart the person approves the rerun of the call, and the rerun
// takes the child that runs instead of starting a second one.
func TestSpawnTaskRerunAfterARestartTakesTheSameChild(t *testing.T) {
	ctx := testCtx(t)
	root := filepath.Join(t.TempDir(), "runs")
	cs := newScripted()
	cs.say("the subtask", endTurn("subtask done"))
	other := newSpawnAgent(t, cs.Model())
	sp := newTaskSpawner(other)
	stopped, release := sp.stopAfterStart()
	cut := &cutLedger{Ledger: openRunsAt(t, root)}
	first := newScripted()
	first.say("parent", callTools(use("c1", "spawn_task", `{"input":"the subtask"}`)))
	a := newSpawnAgent(t, first.Model(), fw.WithLedger(cut), fw.WithSpawner(sp))
	parent, err := a.Start(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("the child never started")
	}
	cut.cut.Store(true) // the parent process dies here
	release()

	second := newScripted()
	second.then("parent", parentGot(t))
	b := newSpawnAgent(t, second.Model(), fw.WithLedger(openRunsAt(t, root)), fw.WithSpawner(sp))
	approveRerunOf(t, ctx, b, parent.ID)
	done, err := b.WaitFor(ctx, parent.ID, func(r *fw.Run) bool { return r.State.Terminal() })
	mustState(t, done, err, fw.StateCompleted)

	list, _ := other.List(ctx)
	if len(list) != 1 {
		t.Fatalf("the child process holds %d runs for one call: %+v", len(list), list)
	}
	if done.Output != "parent got: subtask done" {
		t.Fatalf("parent output = %q", done.Output)
	}
	sp.mu.Lock()
	spawns, abandoned := sp.spawns, sp.abandoned
	sp.mu.Unlock()
	if spawns != 1 || len(abandoned) != 0 {
		t.Fatalf("Spawn ran %d times and abandoned %v", spawns, abandoned)
	}
	kids, err := b.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].ID != list[0].ID || kids[0].Member == "" {
		t.Fatalf("children of the parent = %+v, %v", kids, err)
	}
}

// A child in the same process resumes with its parent after a restart.
// The approved rerun of the open call of spawn_task takes that child again
// instead of starting a second one.
func TestSpawnTaskRerunAfterARestartTakesTheSameLocalChild(t *testing.T) {
	ctx := testCtx(t)
	root := filepath.Join(t.TempDir(), "runs")
	cut := &cutLedger{Ledger: openRunsAt(t, root)}
	started, block := make(chan struct{}), make(chan struct{})
	first := newScripted()
	first.say("parent", callTools(use("c1", "spawn_task", `{"input":"the subtask"}`)))
	first.then("the subtask", func(*fw.Request) *fw.Response {
		close(started)
		<-block
		return endTurn("never seen")
	})
	a := newSpawnAgent(t, first.Model(), fw.WithLedger(cut))
	t.Cleanup(func() { close(block) })
	parent, err := a.Start(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("the child never asked its model")
	}
	cut.cut.Store(true) // the process dies here, with the child at work
	// Nothing of the old process reaches the disk any more, so stopping it
	// changes nothing that the next process reads.
	stop, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	a.Close(stop)
	cancel()

	second := newScripted()
	second.say("the subtask", endTurn("subtask done"))
	second.then("parent", parentGot(t))
	b := newSpawnAgent(t, second.Model(), fw.WithLedger(openRunsAt(t, root)))
	approveRerunOf(t, ctx, b, parent.ID)
	done, err := b.WaitFor(ctx, parent.ID, func(r *fw.Run) bool { return r.State.Terminal() })
	mustState(t, done, err, fw.StateCompleted)
	runs, err := b.List(ctx)
	if err != nil || len(runs) != 2 {
		t.Fatalf("the process holds %d runs for a parent and one call: %+v, %v", len(runs), runs, err)
	}
	if done.Output != "parent got: subtask done" {
		t.Fatalf("parent output = %q", done.Output)
	}
	kids, err := b.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].State != fw.StateCompleted {
		t.Fatalf("children of the parent = %+v, %v", kids, err)
	}
}

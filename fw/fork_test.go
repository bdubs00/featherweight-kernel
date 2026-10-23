package fw

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A branch keeps the events of its source to a point and goes on from
// there; the source does not change.
func TestForkContinuesFromThePrefix(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("first"), endTurn("second"), endTurn("third")).Model())
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if src, err = a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || src.Output != "first" {
		t.Fatalf("source = %+v, %v", src, err)
	}
	evs, err := readAll(ctx, a.ledger, src.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1].Seq

	// A branch at the end with a new message goes on in another direction.
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "and then?", Origin: OriginHTTP})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if b.ID == src.ID || b.Metadata[MetaForkOf] != src.ID || b.Metadata[MetaForkSeq] != strconv.FormatUint(last, 10) {
		t.Fatalf("branch = %+v", b)
	}
	if b, err = a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || b.Output != "second" {
		t.Fatalf("branch = %+v, %v", b, err)
	}
	bevs, err := readAll(ctx, a.ledger, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bevs) <= len(evs) {
		t.Fatalf("the branch has %d events, the source %d; the branch must hold the prefix and its own events", len(bevs), len(evs))
	}
	for i, e := range evs {
		if bevs[i].Type != e.Type || bevs[i].Subject != b.ID || bevs[i].ID == e.ID {
			t.Fatalf("event %d of the branch = %s %s %s; source %s %s", i, bevs[i].Type, bevs[i].Subject, bevs[i].ID, e.Type, e.ID)
		}
	}
	if again, err := a.Get(ctx, src.ID); err != nil || again.Output != "first" {
		t.Fatalf("the source changed: %+v, %v", again, err)
	}

	// A branch before the answer of the model asks the model again.
	var started uint64
	for _, e := range evs {
		if e.Type == EvRunStarted {
			started = e.Seq
		}
	}
	c, err := a.Fork(ctx, src.ID, ForkRequest{Seq: started})
	if err != nil {
		t.Fatalf("Fork at %d: %v", started, err)
	}
	if c, err = a.WaitFor(ctx, c.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil || c.Output != "third" {
		t.Fatalf("early branch = %+v, %v", c, err)
	}
	if c.Metadata[MetaForkSeq] != strconv.FormatUint(started, 10) {
		t.Fatalf("early branch metadata = %v", c.Metadata)
	}
	if list, _ := a.List(ctx); len(list) != 3 {
		t.Fatalf("runs = %d, want the source and two branches", len(list))
	}

	// Errors: a sequence number beyond the ledger, and an unknown run.
	if _, err := a.Fork(ctx, src.ID, ForkRequest{Seq: last + 100}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Fork beyond the ledger = %v", err)
	}
	if _, err := a.Fork(ctx, "01NOPE", ForkRequest{}); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Fork of an unknown run = %v", err)
	}
}

// With spawn, a branch runs in another process through a Forker: the other
// process gets the kept events, the source records the branch as a child,
// and Get reaches the branch on its member.
func TestForkInAnotherProcess(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "try again elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	if b.Member != "other" || b.Metadata[MetaForkOf] != src.ID || b.Parent != src.ID || b.ID == "" {
		t.Fatalf("branch = %+v", b)
	}
	// The source answers for the branch until the other process has it.
	if r, err := a.Get(ctx, b.ID); err != nil || r.ID != b.ID || r.Member != "other" {
		t.Fatalf("Get of the branch while it starts = %+v, %v", r, err)
	}
	got, err := a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.Output != "elsewhere" || got.Member != "other" {
		t.Fatalf("branch in the other process = %+v, %v", got, err)
	}
	if kids, err := a.Children(ctx, src.ID); err != nil || len(kids) != 1 || kids[0].ID != b.ID {
		t.Fatalf("children of the source = %+v, %v", kids, err)
	}
	evs, _ := readAll(ctx, a.ledger, src.ID)
	recorded := false
	for _, e := range evs {
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil && d.Child == b.ID && d.Member == "other" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the source did not record the branch as a child")
	}
	if list, _ := other.List(ctx); len(list) != 1 || list[0].ID != b.ID || list[0].Output != "elsewhere" || list[0].Metadata[MetaForkOf] != src.ID {
		t.Fatalf("the other process holds %+v", list)
	}
	// The same branch sent again is the branch that the other process has.
	evs, _ = readAll(ctx, other.ledger, b.ID)
	again, err := other.ForkEvents(ctx, evs[:1], ForkRequest{ID: b.ID})
	if err != nil || again.ID != b.ID {
		t.Fatalf("the branch sent again = %+v, %v", again, err)
	}
	if list, _ := other.List(ctx); len(list) != 1 {
		t.Fatalf("the other process holds %d runs after the repeat", len(list))
	}
}

// A restart of the source while a branch starts in another process loses
// the branch: at the rebuild the source records it as failed, with the
// reason, and the process that was placed for it is abandoned. A branch
// that had started is not touched.
func TestForkLostInARestart(t *testing.T) {
	ctx := testCtx(t)
	ledger := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	// A first source whose branch did start, in the same ledger.
	ok := &otherSpawner{other: other}
	b := newTestAgent(t, newScript(endTurn("second")).Model(), WithLedger(ledger), WithSpawner(ok))
	src2, err := b.Run(ctx, "hello again")
	mustState(t, src2, err, StateCompleted)
	started, err := b.Fork(ctx, src2.ID, ForkRequest{Input: "sent", Spawn: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.WaitFor(ctx, started.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	// A second source whose branch waits for its process.
	stuck := &otherSpawner{other: other, branchWait: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithLedger(ledger), WithSpawner(stuck))
	src, err := a.Run(ctx, "hello")
	mustState(t, src, err, StateCompleted)
	lost, err := a.Fork(ctx, src.ID, ForkRequest{Input: "never sent", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	// The process goes away while the branch waits for its process, and a
	// new process rebuilds from the ledger.
	a.k.cancel() // no record at shutdown: the ledger keeps the placed branch alone
	sp := &otherSpawner{other: other}
	c := newTestAgent(t, newScript().Model(), WithLedger(ledger), WithSpawner(sp))
	got, err := c.Get(ctx, lost.ID)
	if err != nil || got.State != StateFailed || !strings.Contains(got.Error, "restarted before other took the branch") {
		t.Fatalf("the lost branch after the restart = %+v, %v", got, err)
	}
	if got.Metadata[MetaForkOf] != src.ID || got.Agent != src.Agent {
		t.Fatalf("the record of the lost branch = %+v", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(sp.abandonedMembers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := sp.abandonedMembers(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("abandoned = %v", got)
	}
	if r, err := c.Get(ctx, started.ID); err != nil || r.State != StateCompleted || r.Output != "elsewhere" {
		t.Fatalf("the started branch after the restart = %+v, %v", r, err)
	}
	if got := ok.abandonedMembers(); len(got) != 0 {
		t.Fatalf("the started branch was abandoned: %v", got)
	}
	if c.Starting() != 0 {
		t.Fatalf("Starting = %d after the rebuild", c.Starting())
	}
}

// A branch whose process never takes it is recorded in the source process
// as failed, with the reason, and the source stops counting it as a child
// in another process.
func TestForkInAnotherProcessThatFails(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other, branchErr: errors.New("the Task is Pending after 10m")}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	got, err := a.WaitFor(ctx, b.ID, func(r *Run) bool { return r.State.Terminal() })
	if err != nil || got.State != StateFailed || !strings.Contains(got.Error, "did not start in other") || !strings.Contains(got.Error, "Pending after 10m") {
		t.Fatalf("branch that did not start = %+v, %v", got, err)
	}
	if got.Metadata[MetaForkOf] != src.ID || got.Member != "" {
		t.Fatalf("the record of the branch = %+v", got)
	}
	if list, _ := other.List(ctx); len(list) != 0 {
		t.Fatalf("the other process holds %+v", list)
	}
	if _, ok := a.remoteChild(b.ID); ok {
		t.Fatal("the source still counts the branch as a child in another process")
	}
	// Place that fails is an error of Fork itself, and nothing is recorded.
	sp.placeErr = errors.New("no room")
	if _, err := a.Fork(ctx, src.ID, ForkRequest{Spawn: true}); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("Fork with a Place that fails = %v", err)
	}
}

// Cancel during the start of a branch ends the start, and the branch is
// recorded as canceled.
func TestForkInAnotherProcessCanceledWhileItStarts(t *testing.T) {
	other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
	sp := &otherSpawner{other: other, branchWait: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("here")).Model(), WithSpawner(sp))
	ctx := testCtx(t)
	src, err := a.Start(ctx, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	b, err := a.Fork(ctx, src.ID, ForkRequest{Input: "elsewhere", Spawn: true})
	if err != nil {
		t.Fatalf("Fork with spawn: %v", err)
	}
	if r, err := a.Get(ctx, b.ID); err != nil || r.State != StateQueued {
		t.Fatalf("the branch while it starts = %+v, %v", r, err)
	}
	if a.Starting() != 1 {
		t.Fatalf("Starting = %d while the branch starts", a.Starting())
	}
	if err := a.Decide(ctx, b.ID, ApprovalDecision{ApprovalID: "x", Decision: "allow"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Decide while the branch starts = %v", err)
	}
	if err := a.Cancel(ctx, b.ID); err != nil {
		t.Fatalf("Cancel while the branch starts: %v", err)
	}
	got, err := a.Get(ctx, b.ID)
	if err != nil || got.State != StateCanceled {
		t.Fatalf("the canceled branch = %+v, %v", got, err)
	}
	if list, _ := other.List(ctx); len(list) != 0 {
		t.Fatalf("the other process holds %+v", list)
	}
	if a.Starting() != 0 {
		t.Fatalf("Starting = %d after the start ended", a.Starting())
	}
}

// A branch from events needs a fw.run.queued event first.
func TestForkEventsNeedsAQueuedEvent(t *testing.T) {
	a := newTestAgent(t, newScript(endTurn("x")).Model())
	if _, err := a.ForkEvents(testCtx(t), []Event{{Type: EvRunStarted}}, ForkRequest{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ForkEvents without queued = %v", err)
	}
}

// flakyAbandon is an otherSpawner whose Abandon fails its first fails
// tries, and that paces the tries of a test in milliseconds.
type flakyAbandon struct {
	*otherSpawner
	fails int

	mu    sync.Mutex
	tries int
}

func (s *flakyAbandon) Abandon(ctx context.Context, member string) error {
	s.mu.Lock()
	s.tries++
	fail := s.tries <= s.fails
	s.mu.Unlock()
	if fail {
		return errors.New("the ax API is not reachable yet")
	}
	return s.otherSpawner.Abandon(ctx, member)
}

func (s *flakyAbandon) lookPace() (time.Duration, time.Duration) {
	return time.Millisecond, time.Millisecond
}

func (s *flakyAbandon) triesSoFar() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tries
}

// A rebuild abandons the process of a lost branch before the start of the
// kernel returns, tries again after a failure, and names a process that
// it could not remove in the record of the branch.
func TestLostBranchAbandonsBeforeTheStart(t *testing.T) {
	ctx := testCtx(t)
	for _, c := range []struct {
		fails     int
		tries     int
		abandoned int
		reason    string
	}{
		{fails: 2, tries: 3, abandoned: 1},
		{fails: lookTries, tries: lookTries, abandoned: 0, reason: "its process could not be removed: the ax API is not reachable yet"},
	} {
		ledger := NewMemLedger()
		other := newTestAgent(t, newScript(endTurn("elsewhere")).Model())
		stuck := &otherSpawner{other: other, branchWait: make(chan struct{})}
		a := newTestAgent(t, newScript(endTurn("here")).Model(), WithLedger(ledger), WithSpawner(stuck))
		src, err := a.Run(ctx, "hello")
		mustState(t, src, err, StateCompleted)
		lost, err := a.Fork(ctx, src.ID, ForkRequest{Input: "never sent", Spawn: true})
		if err != nil {
			t.Fatal(err)
		}
		a.k.cancel()
		sp := &flakyAbandon{otherSpawner: &otherSpawner{other: other}, fails: c.fails}
		b := newTestAgent(t, newScript().Model(), WithLedger(ledger), WithSpawner(sp))
		// No wait: the start of the kernel has done both.
		if got := sp.triesSoFar(); got != c.tries || len(sp.abandonedMembers()) != c.abandoned {
			t.Fatalf("after the start: %d tries, abandoned %v; want %d tries and %d abandoned", got, sp.abandonedMembers(), c.tries, c.abandoned)
		}
		got, err := b.Get(ctx, lost.ID)
		if err != nil || got.State != StateFailed || !strings.Contains(got.Error, "restarted before other took the branch") {
			t.Fatalf("the lost branch = %+v, %v", got, err)
		}
		if c.reason == "" && strings.Contains(got.Error, "could not be removed") || !strings.Contains(got.Error, c.reason) {
			t.Fatalf("the reason of the lost branch = %q, want %q", got.Error, c.reason)
		}
	}
}

package fw

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// typeGateLedger holds the first append of one event type until the test
// releases it.
type typeGateLedger struct {
	Ledger
	typ     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newTypeGate(l Ledger, typ string) *typeGateLedger {
	return &typeGateLedger{Ledger: l, typ: typ, entered: make(chan struct{}), release: make(chan struct{})}
}

// releaser returns a function that closes ch once, and registers it as a
// cleanup, so a test that fails before its release does not leave an
// append blocked and its agent unable to close. Call it after the agent
// exists: the cleanup then runs before the close of the agent.
func releaser(t *testing.T, ch chan struct{}) func() {
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

func (l *typeGateLedger) Append(ctx context.Context, e Event) (uint64, error) {
	if e.Type == l.typ {
		first := false
		l.once.Do(func() { first = true; close(l.entered) })
		if first {
			<-l.release
		}
	}
	return l.Ledger.Append(ctx, e)
}

// waitDrivers waits until the kernel counts n admitted root drivers.
func waitDrivers(t *testing.T, a *Agent, n int) {
	t.Helper()
	ctx := testCtx(t)
	for {
		a.k.slotMu.Lock()
		got, changed := a.k.rootDrivers, a.k.slotsChanged
		a.k.slotMu.Unlock()
		if got == n {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("root drivers = %d, want %d", got, n)
		}
	}
}

func rootDrivers(a *Agent) int {
	a.k.slotMu.Lock()
	defer a.k.slotMu.Unlock()
	return a.k.rootDrivers
}

// A drain that starts after fw.run.queued is in the ledger, here while the
// record of an attachment is in flight, does not refuse the start: the run
// is accepted, the shutdown suspends it, and the next start of the process
// resumes it.
func TestStartCutByADrainAfterTheAppendReturnsTheRun(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	base := NewMemLedger()
	l := newTypeGate(base, EvFileChanged)
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithConfig(cfg), WithLedger(l))
	release := releaser(t, l.release)
	type result struct {
		r   *Run
		err error
	}
	got := make(chan result, 1)
	go func() {
		r, err := a.Start(ctx, "work", WithAttachments(Attachment("notes.txt", []byte("notes"))))
		got <- result{r, err}
	}()
	<-l.entered
	closed := make(chan struct{})
	go func() { a.Close(ctx); close(closed) }()
	<-a.k.drainCh
	release()
	res := <-got
	if res.err != nil || res.r == nil {
		t.Fatalf("a start that the drain cut after its append = %+v, %v", res.r, res.err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	types := eventTypes(ledgerEvents(t, base, res.r.ID))
	if strings.Join(types, " ") != strings.Join([]string{EvRunQueued, EvFileChanged, EvRunSuspended}, " ") {
		t.Fatalf("ledger of the accepted run = %v", types)
	}
	if n := rootDrivers(a); n != 0 {
		t.Fatalf("admissions after shutdown = %d", n)
	}
	b := newTestAgent(t, newScript(endTurn("done")).Model(), WithConfig(cfg), WithLedger(base))
	done, err := b.Wait(ctx, res.r.ID)
	mustState(t, done, err, StateCompleted)
}

// The bus intake acknowledges a job whose fw.run.queued reached the ledger
// while the drain began, so the bus does not deliver it to another process,
// and the ledger holds the job.
func TestBusIntakeAcksAJobAcceptedDuringADrain(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.drain"
	qb := newQueueBus(queue)
	base := NewMemLedger()
	l := newTypeGate(base, EvRunQueued)
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithConfig(intakeConfig(queue)), WithBus(qb), WithLedger(l))
	release := releaser(t, l.release)
	e := job(t, RunRequest{Input: "drained job"})
	if err := qb.Publish(ctx, queue, e); err != nil {
		t.Fatal(err)
	}
	<-l.entered
	closed := make(chan struct{})
	go func() { a.Close(ctx); close(closed) }()
	<-a.k.drainCh
	release()
	waitUntil(t, "the ack of the accepted job", func() bool { return qb.isAcked(e.ID) })
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	qb.mu.Lock()
	again := qb.again
	qb.mu.Unlock()
	if again != 0 {
		t.Fatalf("the bus got the accepted job back %d times", again)
	}
	var found bool
	for id, err := range base.Runs(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		evs := ledgerEvents(t, base, id)
		if len(evs) > 0 && evs[len(evs)-1].Type == EvRunSuspended {
			found = true
		}
	}
	if !found {
		t.Fatal("the ledger does not hold the suspended run of the job")
	}
}

// pipeRunner drives the runs whose input is "hosted" and fails each with an
// error that is not a storage fault, as a hosted harness whose pipe broke.
type pipeRunner struct{ calls atomic.Int32 }

func (r *pipeRunner) Drive(_ context.Context, h *Hosted) (bool, error) {
	if h.Queued().Input != "hosted" {
		return false, nil
	}
	r.calls.Add(1)
	return true, errors.New("harness: write to the session: broken pipe")
}

// A driver that stops on an error that is not a storage fault fails its
// run alone. The process stays ready, and other runs work.
func TestDriverErrorFailsTheRunAlone(t *testing.T) {
	ctx := testCtx(t)
	runner := &pipeRunner{}
	a := newTestAgent(t, newScript(endTurn("turn loop done")).Model(), WithRunner(runner))
	r, err := a.Run(ctx, "hosted")
	mustState(t, r, err, StateFailed)
	if !strings.Contains(r.Error, "driver of the run stopped") || strings.Contains(r.Error, "broken pipe") {
		t.Fatalf("failed run error = %q", r.Error)
	}
	if err := a.RuntimeError(); err != nil || !a.Ready() || !a.RuntimeErrorSince().IsZero() {
		t.Fatalf("a driver error fail-stopped the process: %v, ready=%v", err, a.Ready())
	}
	other, err := a.Run(ctx, "plain")
	mustState(t, other, err, StateCompleted)
	if runner.calls.Load() != 1 {
		t.Fatalf("runner calls = %d", runner.calls.Load())
	}
}

// A storage fault fail-stops the process, and the process records the
// time of the failure.
func TestStorageFaultRecordsItsTime(t *testing.T) {
	ctx := testCtx(t)
	cause := errors.New("injected append fault")
	l := &eventFaultLedger{Ledger: NewMemLedger(), typ: EvRunQueued, cause: cause}
	at := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithLedger(l), WithClock(fixedClock{at}))
	if _, err := a.Start(ctx, "work"); !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("start = %v", err)
	}
	if got := a.RuntimeErrorSince(); !got.Equal(at) {
		t.Fatalf("RuntimeErrorSince = %v, want %v", got, at)
	}
	if err := a.failStorage(errors.New("not a storage fault")); isStorageFault(err) || errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("failStorage of an error that is no storage fault = %v", err)
	}
}

// fixedClock is a clock that always reads one time.
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

// A gated job holds its place in the backlog from the moment the gate
// takes it: a direct start in the meantime cannot take the place, and the
// start of the job is never refused for saturation.
func TestBusIntakeGatedJobIsNeverSaturated(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.place"
	qb := newQueueBus(queue)
	cfg := intakeConfig(queue)
	cfg.MaxRuns, cfg.MaxQueuedRuns = 2, 1
	gate := make(chan struct{})
	var entered atomic.Int32
	hold := func(_ context.Context, _ *Config, req *RunRequest, _ *Run) map[string]string {
		if req.Origin == OriginBus {
			entered.Add(1)
			<-gate
		}
		return nil
	}
	a := newTestAgent(t, blockingModel(nil), WithConfig(cfg), WithBus(qb), WithRunAnnotators(hold))
	release := releaser(t, gate)
	jobs := []Event{job(t, RunRequest{Input: "job one"}), job(t, RunRequest{Input: "job two"})}
	for _, e := range jobs {
		if err := qb.Publish(ctx, queue, e); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "both jobs in their start", func() bool { return entered.Load() == 2 })
	if _, err := a.Start(ctx, "direct"); err != nil {
		t.Fatalf("direct start beside two jobs = %v", err)
	}
	release()
	waitUntil(t, "the acks of both jobs", func() bool { return qb.isAcked(jobs[0].ID) && qb.isAcked(jobs[1].ID) })
	qb.mu.Lock()
	again := qb.again
	qb.mu.Unlock()
	if again != 0 {
		t.Fatalf("a job that the gate took went back to the bus %d times", again)
	}
	if _, err := a.Start(ctx, "full"); !errors.Is(err, ErrSaturated) {
		t.Fatalf("start with a full backlog = %v", err)
	}
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

// A job of a bus without a gate waits for a place in the backlog instead
// of failing: the in-memory bus drops a message after three failures of
// its handler.
func TestBusIntakeWithoutAGateWaitsForAPlace(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.wait"
	cfg := intakeConfig(queue)
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	mb := NewMemBus()
	a := newTestAgent(t, blockingModel(nil), WithConfig(cfg), WithBus(mb))
	first, err := a.Start(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, "second"); err != nil {
		t.Fatal(err)
	}
	if err := mb.Publish(ctx, queue, job(t, RunRequest{Input: "waiting job"})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if r := runByInput(t, a, "waiting job"); r != nil {
		t.Fatalf("the job started past the bound: %+v", r)
	}
	if err := a.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the run of the job", func() bool { return runByInput(t, a, "waiting job") != nil })
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

// A job whose start ends with the kernel context, here at a shutdown while
// it waits for the acceptance of the run under its key, goes back to the
// bus. The dead letter gets no record of a valid job.
func TestBusIntakeKernelContextEndIsNoDeadLetter(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.ctx"
	mb := NewMemBus()
	dead := deadRecords(t, mb, queue)
	l := &acceptanceLedger{Ledger: NewMemLedger(), entered: make(chan struct{}), release: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithConfig(intakeConfig(queue)), WithBus(mb), WithLedger(l))
	release := releaser(t, l.release)
	owner := make(chan error, 1)
	go func() { _, err := a.Start(ctx, "keyed", WithIdempotencyKey("same")); owner <- err }()
	<-l.entered
	if err := mb.Publish(ctx, queue, job(t, RunRequest{Input: "keyed", IdempotencyKey: "same"})); err != nil {
		t.Fatal(err)
	}
	// The handler holds a place of the backlog while it starts the job, and
	// then waits for the acceptance of the run under its key.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		a.k.slotMu.Lock()
		n := a.k.admitted
		a.k.slotMu.Unlock()
		if n == 1 {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	closed := make(chan struct{})
	go func() { a.Close(sctx); close(closed) }()
	<-sctx.Done()
	noRecord(t, dead)
	release()
	if err := <-owner; err != nil {
		t.Fatalf("the owner of the key = %v", err)
	}
	<-closed
	noRecord(t, dead)
}

// A decision and a follow-up continue runs that the process accepted, so a
// full backlog refuses neither. Each takes a place for its driver, which
// waits for a working slot.
func TestContinuationIsExemptFromTheBacklogBound(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	var calls atomic.Int32
	m := ModelFunc{GenerateFn: func(ctx context.Context, _ *Request) (*Response, error) {
		if calls.Add(1) == 1 {
			return endTurn("done"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	a := newTestAgent(t, m, WithConfig(cfg))
	finished, err := a.Run(ctx, "finish")
	mustState(t, finished, err, StateCompleted)
	waitDrivers(t, a, 0)
	// A branch that waits for a decision holds no place.
	branch := NewID()
	kept := []Event{
		cursorEvent(t, 1, EvRunQueued, RunQueuedData{Agent: "default", Input: "ask me", Origin: OriginHTTP}),
		cursorEvent(t, 2, EvRunStarted, RunStartedData{Model: "anthropic/claude-opus-5-5"}),
		cursorEvent(t, 3, EvTurnStarted, TurnData{Turn: 1}),
		cursorEvent(t, 4, EvModelRequested, ModelRequestedData{}),
		cursorEvent(t, 5, EvModelResponded, ModelRespondedData{StopReason: StopToolUse, Content: []Block{{Type: BlockToolUse, ID: "c1", Name: "ask", Input: json.RawMessage(`{}`)}}}),
		cursorEvent(t, 6, EvToolRequested, ToolRequestedData{Tool: "ask", CallID: "c1", Input: json.RawMessage(`{}`)}),
		cursorEvent(t, 7, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", CallID: "c1", Tool: "ask", Input: json.RawMessage(`{}`)}),
	}
	if r, err := a.ForkEvents(ctx, kept, ForkRequest{ID: branch}); err != nil || r.State != StateInputRequired {
		t.Fatalf("waiting branch = %+v, %v", r, err)
	}
	for range 2 {
		if _, err := a.Start(ctx, "block"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Start(ctx, "full"); !errors.Is(err, ErrSaturated) {
		t.Fatalf("new start with a full backlog = %v", err)
	}
	err = a.Send(ctx, finished.ID, Message{Content: []Block{Text("again")}}, OriginHTTP, WithSendBudget(Budget{MaxTurns: 3}))
	if err != nil {
		t.Fatalf("follow-up with a full backlog = %v", err)
	}
	evs := ledgerEvents(t, a.Ledger(), finished.ID)
	if n := len(evs); n < 2 || evs[n-2].Type != EvBudgetChanged || evs[n-1].Type != EvMessageReceived {
		t.Fatalf("the follow-up wrote %v", eventTypes(evs))
	}
	if err := a.Decide(ctx, branch, ApprovalDecision{ApprovalID: "ap1", Decision: "allow"}); err != nil {
		t.Fatalf("decision with a full backlog = %v", err)
	}
	if n := rootDrivers(a); n != 4 {
		t.Fatalf("root admissions = %d, want the two starts, the follow-up, and the decision", n)
	}
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

// A start that the full backlog refuses stores none of its attachments.
func TestRefusedStartLeavesNoFileOnDisk(t *testing.T) {
	ctx := testCtx(t)
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = filepath.Join(root, ".fw")
	cfg.Tools.Files.Root = root
	on := true
	cfg.Tools.Files.PerRun = &on
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	a := newTestAgent(t, blockingModel(nil), WithConfig(cfg))
	for range 2 {
		if _, err := a.Start(ctx, "block"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := a.Start(ctx, "full", WithAttachments(Attachment("refused.txt", []byte("private notes"))))
	if !errors.Is(err, ErrSaturated) {
		t.Fatalf("start with a full backlog = %v", err)
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "refused.txt" {
			t.Errorf("a refused start left %s", path)
		}
		return nil
	})
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

// countingForker counts the processes that Place makes.
type countingForker struct {
	*otherSpawner
	places atomic.Int32
}

func (f *countingForker) Place(ctx context.Context, r *Run) (string, error) {
	f.places.Add(1)
	return f.otherSpawner.Place(ctx, r)
}

// A drain that begins while a remote fork is checked refuses the fork
// before Place, so no process exists for it in the fleet.
func TestForkChecksTheDrainBeforePlace(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &countingForker{otherSpawner: &otherSpawner{other: other}}
	var armed atomic.Bool
	entered, gate := make(chan struct{}), make(chan struct{})
	authz := AuthzFunc(func(_ context.Context, _ *Principal, action string, _ Target) error {
		if action == ActionAgentRun && armed.CompareAndSwap(true, false) {
			close(entered)
			<-gate
		}
		return nil
	})
	a := newTestAgent(t, newScript(endTurn("source done")).Model(), WithSpawner(sp), WithAuthz(authz))
	release := releaser(t, gate)
	source, err := a.Run(ctx, "source work")
	mustState(t, source, err, StateCompleted)
	armed.Store(true)
	pctx := WithPrincipal(ctx, &Principal{Subject: "ada@example.com", Kind: KindUser})
	res := make(chan error, 1)
	go func() {
		_, err := a.Fork(pctx, source.ID, ForkRequest{Spawn: true, Input: "go on"})
		res <- err
	}()
	<-entered
	closed := make(chan struct{})
	go func() { a.Close(ctx); close(closed) }()
	<-a.k.drainCh
	release()
	if err := <-res; !errors.Is(err, ErrClosed) {
		t.Fatalf("fork during the drain = %v", err)
	}
	<-closed
	if n := sp.places.Load(); n != 0 {
		t.Fatalf("Place made %d processes during the drain", n)
	}
	if got := sp.abandonedMembers(); len(got) != 0 {
		t.Fatalf("abandoned = %v", got)
	}
}

// After a storage failure, a fork says that storage failed, not that the
// agent shuts down.
func TestForkAfterAStorageFailureNamesTheStorage(t *testing.T) {
	ctx := testCtx(t)
	cause := errors.New("injected append fault")
	l := &eventFaultLedger{Ledger: NewMemLedger(), typ: EvRunChild, cause: cause}
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := &countingForker{otherSpawner: &otherSpawner{other: other}}
	a := newTestAgent(t, newScript(endTurn("source done")).Model(), WithLedger(l), WithSpawner(sp))
	source, err := a.Run(ctx, "source work")
	mustState(t, source, err, StateCompleted)
	if _, err := a.Fork(ctx, source.ID, ForkRequest{Spawn: true}); !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("fork whose record fails = %v", err)
	}
	// The record failed after Place: the abandon is done when Fork returns.
	if got := sp.abandonedMembers(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("abandoned when Fork returned = %v", got)
	}
	for _, req := range []ForkRequest{{}, {Spawn: true}} {
		_, err := a.Fork(ctx, source.ID, req)
		if !errors.Is(err, ErrStorageUnavailable) || errors.Is(err, ErrClosed) {
			t.Fatalf("fork %+v after the failure = %v", req, err)
		}
	}
	if _, err := a.ForkEvents(ctx, ledgerEvents(t, l, source.ID), ForkRequest{ID: NewID()}); !errors.Is(err, ErrStorageUnavailable) || errors.Is(err, ErrClosed) {
		t.Fatalf("ForkEvents after the failure = %v", err)
	}
	if n := sp.places.Load(); n != 1 {
		t.Fatalf("Place ran %d times, want only the first fork", n)
	}
}

// A branch takes its place in the backlog before its first append and
// holds it through the copy. A copied fw.run.finished does not give the
// place to another start, so a later copy, such as the message of a
// follow-up of the source, never meets a full backlog, and the ledger never
// holds a part of a branch.
func TestBranchHoldsItsPlaceThroughTheCopy(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	var calls atomic.Int32
	m := ModelFunc{GenerateFn: func(ctx context.Context, _ *Request) (*Response, error) {
		switch calls.Add(1) {
		case 1:
			return endTurn("first"), nil
		case 2:
			return endTurn("second"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	a := newTestAgent(t, m, WithConfig(cfg))
	src, err := a.Run(ctx, "source")
	mustState(t, src, err, StateCompleted)
	if err := a.Send(ctx, src.ID, Message{Content: []Block{Text("again")}}, OriginHTTP); err != nil {
		t.Fatal(err)
	}
	done, err := a.WaitFor(ctx, src.ID, func(r *Run) bool { return r.State == StateCompleted && r.Output == "second" })
	mustState(t, done, err, StateCompleted)
	waitDrivers(t, a, 0)
	if _, err := a.Start(ctx, "block"); err != nil {
		t.Fatal(err)
	}
	branch := NewID()
	stole := make(chan error, 1)
	var once sync.Once
	a.Observe(ObserverFunc(func(e Event) {
		if e.Subject == branch && e.Type == EvRunFinished {
			once.Do(func() {
				_, err := a.Start(context.Background(), "steal")
				stole <- err
			})
		}
	}))
	r, err := a.Fork(ctx, src.ID, ForkRequest{ID: branch})
	if err != nil {
		t.Fatalf("fork = %v; the ledger holds %v", err, eventTypes(ledgerEvents(t, a.Ledger(), branch)))
	}
	if r.State != StateCompleted {
		t.Fatalf("branch = %+v", r)
	}
	if err := <-stole; !errors.Is(err, ErrSaturated) {
		t.Fatalf("a start during the copy took the place of the branch: %v", err)
	}
	if got, want := len(ledgerEvents(t, a.Ledger(), branch)), len(ledgerEvents(t, a.Ledger(), src.ID)); got != want {
		t.Fatalf("the branch holds %d events, want the %d copies", got, want)
	}
	waitDrivers(t, a, 1)
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

// A shutdown or a storage failure that cuts the start of a child in another
// process keeps the process of the child, which can have taken its run, and
// records nothing. The look after the restart finds the child and follows
// it, so the approved rerun of the call takes it again instead of starting
// a second child.
func TestSpawnChildKeepsTheChildWhenTheKernelEnds(t *testing.T) {
	for _, how := range []string{"shutdown", "storage failure"} {
		t.Run(how, func(t *testing.T) {
			ctx := testCtx(t)
			other := newTestAgent(t, blockingModel(nil))
			sp := newCrashSpawner(other)
			root := filepath.Join(t.TempDir(), "runs")
			cut := &cutLedger{Ledger: openFileLedgerAt(t, root)}
			reached, release := sp.stop(beforeEnd)
			defer release()
			a := newTestAgent(t, newScript(callTools(use("c1", "spawn", `{}`))).Model(), WithLedger(cut), WithSpawner(sp), WithTools(spawnTool()))
			parent, err := a.Start(ctx, "parent work")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("Spawn never got to the boundary")
			}
			ids, members := recordedChildren(t, cut, parent.ID)
			if len(ids) != 1 || members[ids[0]] == "" {
				t.Fatalf("records = %v %v", ids, members)
			}
			child, member := ids[0], members[ids[0]]
			if how == "shutdown" {
				sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
				a.Close(sctx)
			} else {
				cut.cut.Store(true)
				if _, err := a.Start(ctx, "trigger"); !errors.Is(err, ErrStorageUnavailable) {
					t.Fatalf("start after the cut = %v", err)
				}
				a.Close(ctx)
			}
			if _, created, abandoned := sp.counts(); len(created) != 1 || len(abandoned) != 0 || sp.alive(member) != nil {
				t.Fatalf("created %v, abandoned %v: the cut start lost its process", created, abandoned)
			}
			for _, e := range ledgerEvents(t, cut.Ledger, parent.ID) {
				var d RunChildData
				if e.Type == EvRunChild && e.Decode(&d) == nil && (d.Ended != "" || d.Started) {
					t.Fatalf("the parent recorded %+v for the cut start", d)
				}
			}
			if evs := ledgerEvents(t, cut.Ledger, child); len(evs) != 0 {
				t.Fatalf("the parent recorded the child as a run here: %v", eventTypes(evs))
			}
			if err := cut.Ledger.(*FileLedger).Close(); err != nil {
				t.Fatal(err)
			}
			l := openFileLedgerAt(t, root)
			b := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithSpawner(sp), WithTools(spawnTool()))
			eventually(t, "the child is found in its process", func() bool {
				r, err := b.Get(ctx, child)
				return err == nil && r.State == StateRunning && r.Member == member
			})
			approveRerun(t, b, parent.ID)
			eventually(t, "the rerun waits for the child", func() bool {
				r, _ := b.Get(ctx, parent.ID)
				return r != nil && r.State == StateRunning && r.Pending == nil
			})
			if err := b.Cancel(ctx, child); err != nil {
				t.Fatalf("Cancel the found child: %v", err)
			}
			done, err := b.WaitFor(ctx, parent.ID, func(r *Run) bool { return r.State.Terminal() })
			mustState(t, done, err, StateCompleted)
			if spawns, created, abandoned := sp.counts(); spawns != 1 || len(created) != 1 || len(abandoned) != 0 {
				t.Fatalf("Spawn ran %d times, created %v, abandoned %v", spawns, created, abandoned)
			}
		})
	}
}

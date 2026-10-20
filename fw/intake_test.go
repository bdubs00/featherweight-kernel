package fw

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queueBus is a durable queue in memory, as the nats and redis drivers
// are. Publish keeps each message of a queue until a consumer takes it. A
// consumer takes one message at a time, and with a gate only when the gate
// admits one. A handler error puts the message back. The stop function
// waits for the handlers in flight. Every subject also reaches the
// subscribers of the embedded MemBus.
type queueBus struct {
	*MemBus

	mu      sync.Mutex
	queues  map[string][]Event // messages that no consumer took, by queue
	acked   []string           // the ids of the acknowledged messages
	again   int                // messages that a handler error put back
	changed chan struct{}
}

func newQueueBus(queues ...string) *queueBus {
	b := &queueBus{MemBus: NewMemBus(), queues: map[string][]Event{}, changed: make(chan struct{})}
	for _, q := range queues {
		b.queues[q] = nil
	}
	return b
}

func (b *queueBus) Publish(ctx context.Context, subject string, e Event) error {
	b.mu.Lock()
	if _, ok := b.queues[subject]; ok {
		b.queues[subject] = append(b.queues[subject], e)
		b.signalLocked()
	}
	b.mu.Unlock()
	return b.MemBus.Publish(ctx, subject, e)
}

func (b *queueBus) signalLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// take waits for the next message of the queue.
func (b *queueBus) take(ctx context.Context, queue string) (Event, bool) {
	for {
		b.mu.Lock()
		if q := b.queues[queue]; len(q) > 0 {
			e := q[0]
			b.queues[queue] = q[1:]
			b.mu.Unlock()
			return e, true
		}
		ch := b.changed
		b.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Event{}, false
		}
	}
}

func (b *queueBus) settle(queue string, e Event, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.queues[queue] = append([]Event{e}, b.queues[queue]...)
		b.again++
	} else {
		b.acked = append(b.acked, e.ID)
	}
	b.signalLocked()
}

// Consume handles one message at a time, on the loop of the consumer.
func (b *queueBus) Consume(_ context.Context, queue string, h func(Event) error) (func(), error) {
	return b.consume(queue, nil, h), nil
}

// ConsumeGated asks the gate before it takes a message, and runs each
// handler on a goroutine of its own, as the drivers do.
func (b *queueBus) ConsumeGated(_ context.Context, queue string, g Gate, h func(Event) error) (func(), error) {
	return b.consume(queue, g, h), nil
}

func (b *queueBus) consume(queue string, g Gate, h func(Event) error) func() {
	ctx, cancel := context.WithCancel(context.Background())
	var handlers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if g != nil {
				if _, err := g.Admit(ctx, 1); err != nil {
					return
				}
			}
			e, ok := b.take(ctx, queue)
			if !ok {
				if g != nil {
					g.Unused(1)
				}
				return
			}
			if g == nil {
				b.settle(queue, e, h(e))
				continue
			}
			handlers.Add(1)
			go func() {
				defer handlers.Done()
				b.settle(queue, e, h(e))
			}()
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			handlers.Wait()
		})
	}
}

func (b *queueBus) waiting(queue string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queues[queue])
}

func (b *queueBus) isAcked(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Contains(b.acked, id)
}

func (b *queueBus) ackCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.acked)
}

// plainBus hides ConsumeGated, as a bus driver without a gate does.
type plainBus struct{ Bus }

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func job(t *testing.T, req RunRequest) Event {
	t.Helper()
	return MustEvent("", EvRunRequest, req)
}

func intakeConfig(queue string) *Config {
	cfg := DefaultConfig()
	cfg.Bus.Intake = queue
	cfg.MaxRuns = 1
	cfg.Agents = map[string]AgentProfile{"default": {}}
	return cfg
}

// inputOf returns the text of the first message of a model request.
func inputOf(req *Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	return TextOf(req.Messages[0].Content)
}

func runByInput(t *testing.T, a *Agent, input string) *Run {
	t.Helper()
	runs, err := a.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Input == input {
			return r
		}
	}
	return nil
}

// With maxRuns 1, the intake acknowledges a job as soon as its run is in
// the ledger, and takes the next job only when the run slot is free: here
// when the first run waits for an approval. The second job stays on the
// queue for the other consumers until then.
func TestBusIntakeTakesAJobForAFreeSlot(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.slot"
	qb := newQueueBus(queue)
	one := job(t, RunRequest{Input: "job one"})
	two := job(t, RunRequest{Input: "job two"})
	for _, e := range []Event{one, two} {
		if err := qb.Publish(ctx, queue, e); err != nil {
			t.Fatal(err)
		}
	}
	started := make(chan string, 4)
	release := make(chan struct{})
	m := ModelFunc{StreamFn: func(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
		in := inputOf(req)
		started <- in
		if strings.Contains(in, "job two") {
			return endTurn("two done"), nil
		}
		select {
		case <-release:
			return callTools(use("c1", "rm", `{}`)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	var calls atomic.Int32
	a := newTestAgent(t, m, WithConfig(intakeConfig(queue)), WithBus(qb), WithTools(destructiveTool(&calls)))

	if in := <-started; !strings.Contains(in, "job one") {
		t.Fatalf("first model call for %q", in)
	}
	waitUntil(t, "the ack of the first job", func() bool { return qb.isAcked(one.ID) })
	time.Sleep(200 * time.Millisecond)
	if n := qb.waiting(queue); n != 1 {
		t.Fatalf("messages on the queue while the only slot works = %d, want 1", n)
	}
	if runs, _ := a.List(ctx); len(runs) != 1 {
		t.Fatalf("runs while the only slot works = %d, want 1", len(runs))
	}

	// The first run waits for a person and gives its slot up.
	close(release)
	waitUntil(t, "the second run", func() bool {
		r := runByInput(t, a, "job two")
		return r != nil && r.State == StateCompleted
	})
	if !qb.isAcked(two.ID) || qb.waiting(queue) != 0 {
		t.Fatalf("acked = %v, waiting = %d", qb.acked, qb.waiting(queue))
	}
	if r := runByInput(t, a, "job one"); r == nil || r.State != StateInputRequired {
		t.Fatalf("first run = %+v", r)
	}
	if calls.Load() != 0 {
		t.Fatal("the held tool ran")
	}
}

// A bus without a gate gets the same acknowledgement: when the run is in
// the ledger, not when it ends.
func TestBusIntakeAcksAtStartWithoutAGate(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.plain"
	qb := newQueueBus(queue)
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started), WithConfig(intakeConfig(queue)), WithBus(plainBus{qb}))
	e := job(t, RunRequest{Input: "long job"})
	if err := qb.Publish(ctx, queue, e); err != nil {
		t.Fatal(err)
	}
	<-started
	waitUntil(t, "the ack", func() bool { return qb.isAcked(e.ID) })
	if r := runByInput(t, a, "long job"); r == nil || r.State != StateRunning {
		t.Fatalf("run = %+v", r)
	}
	sctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	a.Close(sctx)
}

// A shutdown with a job in flight does not wait for its run, with a bus
// whose stop waits for its handlers. A run that waits for an approval
// never ends at the drain, so a handler that waited for it hung the stop.
func TestBusIntakeShutdownDoesNotWaitForTheRun(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.stop"
	for _, gated := range []bool{true, false} {
		qb := newQueueBus(queue)
		var bus Bus = qb
		if !gated {
			bus = plainBus{qb}
		}
		var calls atomic.Int32
		a := newTestAgent(t, newScript(callTools(use("c1", "rm", `{}`))).Model(), WithConfig(intakeConfig(queue)), WithBus(bus), WithTools(destructiveTool(&calls)))
		e := job(t, RunRequest{Input: "needs a person"})
		if err := qb.Publish(ctx, queue, e); err != nil {
			t.Fatal(err)
		}
		waitUntil(t, "the approval", func() bool {
			r := runByInput(t, a, "needs a person")
			return r != nil && r.State == StateInputRequired
		})
		done := make(chan struct{})
		go func() {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.Close(sctx)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("gated %v: the shutdown waits for a run that waits for a person", gated)
		}
		if !qb.isAcked(e.ID) || qb.again != 0 || qb.waiting(queue) != 0 {
			t.Fatalf("gated %v: acked %v, again %d, waiting %d", gated, qb.acked, qb.again, qb.waiting(queue))
		}
	}
}

// A run that a shutdown interrupts resumes from the ledger at the next
// start, and the bus does not give its job again: the next process takes
// the run and starts no second one.
func TestBusIntakeRunResumesAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.restart"
	qb := newQueueBus(queue)
	l := NewMemLedger()
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started), WithConfig(intakeConfig(queue)), WithBus(qb), WithLedger(l))
	e := job(t, RunRequest{Input: "long job"})
	if err := qb.Publish(ctx, queue, e); err != nil {
		t.Fatal(err)
	}
	<-started
	sctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	a.Close(sctx)
	cancel()
	r := runByInput(t, a, "long job")
	if r == nil || r.State != StateSuspended {
		t.Fatalf("run after the shutdown = %+v", r)
	}

	b := newTestAgent(t, newScript(endTurn("finished later")).Model(), WithConfig(intakeConfig(queue)), WithBus(qb), WithLedger(l))
	got, err := b.Wait(ctx, r.ID)
	mustState(t, got, err, StateCompleted)
	time.Sleep(100 * time.Millisecond)
	if runs, _ := b.List(ctx); len(runs) != 1 {
		t.Fatalf("runs after the restart = %d, want 1", len(runs))
	}
	if qb.ackCount() != 1 || qb.again != 0 || qb.waiting(queue) != 0 {
		t.Fatalf("acked %v, again %d, waiting %d", qb.acked, qb.again, qb.waiting(queue))
	}
}

// deadRecords subscribes to the dead letter of a queue.
func deadRecords(t *testing.T, b Bus, queue string) <-chan Event {
	t.Helper()
	ch := make(chan Event, 16)
	stop, err := b.Subscribe(context.Background(), queue+DeadLetterSuffix, func(e Event) error {
		ch <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return ch
}

func nextRecord(t *testing.T, ch <-chan Event) (Event, IntakeDeadData) {
	t.Helper()
	select {
	case e := <-ch:
		var d IntakeDeadData
		if err := e.Decode(&d); err != nil {
			t.Fatal(err)
		}
		return e, d
	case <-time.After(5 * time.Second):
		t.Fatal("no dead-letter record")
		return Event{}, IntakeDeadData{}
	}
}

func noRecord(t *testing.T, ch <-chan Event) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected dead-letter record %s", e.Data)
	case <-time.After(200 * time.Millisecond):
	}
}

// A failed job goes on the queue again up to bus.intakeRetries times, as a
// new run that names its attempt and the failed run. After the last
// attempt the dead letter gets a record, and each run records what the
// kernel did.
func TestBusIntakeRetriesAndDeadLetters(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.retry"
	qb := newQueueBus(queue)
	dead := deadRecords(t, qb, queue)
	retries := make(chan Event, 4)
	stop, _ := qb.Subscribe(ctx, queue, func(e Event) error {
		if e.Type == EvRunRequest && strings.HasSuffix(e.ID, ".retry") {
			retries <- e
		}
		return nil
	})
	defer stop()
	cfg := intakeConfig(queue)
	cfg.Bus.IntakeRetries = 1
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), callTools(use("c2", "echo", `{"text":"b"}`)))
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithBus(qb), WithTools(echoTool(nil)))
	e := job(t, RunRequest{Input: "flaky job", Principal: "alice", Budget: failingBudget(), Metadata: map[string]string{"ticket": "7", MetaBatch: "b1"}})
	if err := qb.Publish(ctx, queue, e); err != nil {
		t.Fatal(err)
	}

	rec, d := nextRecord(t, dead)
	runs, _ := a.List(ctx)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(runs))
	}
	first, second := runs[1], runs[0] // newest first
	if first.State != StateFailed || second.State != StateFailed {
		t.Fatalf("states = %s, %s", first.State, second.State)
	}
	waitUntil(t, "the outcomes", func() bool {
		f, _ := a.Get(ctx, first.ID)
		s, _ := a.Get(ctx, second.ID)
		return f.Metadata[MetaIntakeOutcome] == IntakeRetried && s.Metadata[MetaIntakeOutcome] == IntakeDeadLettered
	})
	if first.Metadata[MetaIntakeAttempt] != "" || second.Metadata[MetaIntakeAttempt] != "2" || second.Metadata[MetaIntakeRetryOf] != first.ID {
		t.Fatalf("metadata = %v, %v", first.Metadata, second.Metadata)
	}
	if second.Metadata["ticket"] != "7" || second.Metadata[MetaBatch] != "b1" || second.Principal != "alice" || second.Metadata[metaIdempotencyKey] != first.ID+".retry" {
		t.Fatalf("the retry lost the request: %+v", second)
	}
	retry := <-retries
	var rr RunRequest
	if err := retry.Decode(&rr); err != nil || rr.Input != "flaky job" || rr.Budget == nil || rr.Budget.MaxTurns != 1 {
		t.Fatalf("retry = %s (%v)", retry.Data, err)
	}

	if rec.Type != EvIntakeDead || rec.Subject != second.ID || rec.Node != a.Node() || rec.ID != second.ID+DeadLetterSuffix || rec.Source != "fw://local/default/"+second.ID {
		t.Fatalf("record envelope = %+v", rec)
	}
	if d.Queue != queue || d.Run != second.ID || d.Outcome != DeadFailed || d.Attempts != 2 || d.Error != "budget exceeded: maxTurns" || d.Node != a.Node() || d.Message != "" {
		t.Fatalf("record = %+v", d)
	}
	var req RunRequest
	if err := json.Unmarshal(d.Request, &req); err != nil {
		t.Fatal(err)
	}
	if req.Input != "flaky job" || req.Principal != "alice" || req.Metadata["ticket"] != "7" || req.Metadata[MetaIntakeAttempt] != "2" ||
		req.Metadata[MetaIntakeRetryOf] != first.ID || req.Metadata[MetaIntakeQueue] != "" || req.Metadata[metaIdempotencyKey] != "" || req.IdempotencyKey != first.ID+".retry" {
		t.Fatalf("record request = %s", d.Request)
	}
	noRecord(t, dead)
}

// Without bus.intakeRetries, the first failure goes to the dead letter. A
// canceled run and a completed run owe nothing.
func TestBusIntakeDeadLettersTheFirstFailure(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.once"
	qb := newQueueBus(queue)
	dead := deadRecords(t, qb, queue)
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)), endTurn("fine"))
	a := newTestAgent(t, s.Model(), WithConfig(intakeConfig(queue)), WithBus(qb), WithTools(echoTool(nil)))
	if err := qb.Publish(ctx, queue, job(t, RunRequest{Input: "fails", Budget: failingBudget()})); err != nil {
		t.Fatal(err)
	}
	_, d := nextRecord(t, dead)
	if d.Attempts != 1 || d.Outcome != DeadFailed {
		t.Fatalf("record = %+v", d)
	}
	if err := qb.Publish(ctx, queue, job(t, RunRequest{Input: "works"})); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the second run", func() bool {
		r := runByInput(t, a, "works")
		return r != nil && r.State == StateCompleted
	})
	noRecord(t, dead)
	if runs, _ := a.List(ctx); len(runs) != 2 {
		t.Fatalf("runs = %d, want 2: a retry ran without bus.intakeRetries", len(runs))
	}
}

// A request that cannot start goes to the dead letter with the error and
// is acknowledged, so it does not come back. The record leaves out the
// bytes of the files.
func TestBusIntakeDeadLettersARequestThatCannotStart(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.bad"
	qb := newQueueBus(queue)
	dead := deadRecords(t, qb, queue)
	a := newTestAgent(t, newScript().Model(), WithConfig(intakeConfig(queue)), WithBus(qb))

	unknown := job(t, RunRequest{Input: "for nobody", Agent: "missing", Files: []Block{Attachment("a.txt", []byte("secret bytes"))}, Metadata: map[string]string{MetaIntakeAttempt: "3"}})
	if err := qb.Publish(ctx, queue, unknown); err != nil {
		t.Fatal(err)
	}
	rec, d := nextRecord(t, dead)
	if d.Outcome != DeadRejected || d.Message != unknown.ID || d.Run != "" || d.Attempts != 3 || !strings.Contains(d.Error, ErrAgentNotFound.Error()) || rec.ID != unknown.ID+DeadLetterSuffix {
		t.Fatalf("record = %+v", d)
	}
	if strings.Contains(string(d.Request), "secret") || strings.Contains(string(d.Request), "c2VjcmV0") {
		t.Fatalf("the record holds the bytes of a file: %s", d.Request)
	}
	var req RunRequest
	if err := json.Unmarshal(d.Request, &req); err != nil || req.Agent != "missing" || len(req.Files) != 1 || req.Files[0].File.Name != "a.txt" || req.Origin != "" {
		t.Fatalf("record request = %s (%v)", d.Request, err)
	}

	text := Event{SpecVersion: SpecVersion, ID: NewID(), Type: EvRunRequest, Data: json.RawMessage(`"just text"`)}
	if err := qb.Publish(ctx, queue, text); err != nil {
		t.Fatal(err)
	}
	_, d = nextRecord(t, dead)
	if d.Outcome != DeadRejected || string(d.Request) != `"just text"` || !strings.Contains(d.Error, "not a run request") {
		t.Fatalf("record = %+v", d)
	}
	waitUntil(t, "the acks", func() bool { return qb.isAcked(unknown.ID) && qb.isAcked(text.ID) })
	if qb.again != 0 {
		t.Fatalf("messages delivered again = %d", qb.again)
	}
	if runs, _ := a.List(ctx); len(runs) != 0 {
		t.Fatalf("runs = %d", len(runs))
	}
}

// refusingBus refuses every publish to one subject.
type refusingBus struct {
	Bus
	subject string
	refuse  atomic.Bool
}

func (b *refusingBus) Publish(ctx context.Context, subject string, e Event) error {
	if subject == b.subject && b.refuse.Load() {
		return errors.New("refused")
	}
	return b.Bus.Publish(ctx, subject, e)
}

// A dead letter that refuses the record of a request that cannot start
// has the bus deliver the message again.
func TestBusIntakeRejectWaitsForTheDeadLetter(t *testing.T) {
	const queue = "fw.jobs.wait"
	qb := newQueueBus(queue)
	rb := &refusingBus{Bus: qb, subject: queue + DeadLetterSuffix}
	rb.refuse.Store(true)
	a := newTestAgent(t, newScript().Model(), WithConfig(intakeConfig(queue)), WithBus(rb))
	e := job(t, RunRequest{Input: "x", Agent: "missing"})
	if err := a.handleIntake(queue, e); err == nil {
		t.Fatal("the handler acknowledged a request whose record the dead letter refused")
	}
	rb.refuse.Store(false)
	if err := a.handleIntake(queue, e); err != nil {
		t.Fatalf("handler = %v", err)
	}
}

// With bus.deadLetter false, no record goes out, the message is
// acknowledged, and the failed run records that the kernel dropped it.
func TestBusIntakeDeadLetterOff(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.off"
	qb := newQueueBus(queue)
	dead := deadRecords(t, qb, queue)
	cfg := intakeConfig(queue)
	off := false
	cfg.Bus.DeadLetter = &off
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)))
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithBus(qb), WithTools(echoTool(nil)))
	bad := job(t, RunRequest{Input: "x", Agent: "missing"})
	fails := job(t, RunRequest{Input: "fails", Budget: failingBudget()})
	for _, e := range []Event{bad, fails} {
		if err := qb.Publish(ctx, queue, e); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "the dropped outcome", func() bool {
		r := runByInput(t, a, "fails")
		return r != nil && r.Metadata[MetaIntakeOutcome] == IntakeDropped
	})
	noRecord(t, dead)
	if !qb.isAcked(bad.ID) || !qb.isAcked(fails.ID) {
		t.Fatalf("acked = %v", qb.acked)
	}
}

// A failed job whose record did not go out before a shutdown gets it at
// the next start, from the ledger. A failed run from the bus of an older
// kernel, which names no intake queue, owes nothing.
func TestBusIntakeSendsWhatAFailureOwesAfterARestart(t *testing.T) {
	ctx := testCtx(t)
	const queue = "fw.jobs.later"
	qb := newQueueBus(queue)
	dead := deadRecords(t, qb, queue)
	l := NewMemLedger()
	old := MustEvent("OLD", EvRunQueued, RunQueuedData{Agent: "default", Input: "old", Origin: OriginBus})
	seed(t, l, "OLD", old, MustEvent("OLD", EvRunFinished, RunFinishedData{Outcome: OutcomeFailed, Error: "old failure"}))

	rb := &refusingBus{Bus: qb, subject: queue + DeadLetterSuffix}
	rb.refuse.Store(true)
	s := newScript(callTools(use("c1", "echo", `{"text":"a"}`)))
	a := newTestAgent(t, s.Model(), WithConfig(intakeConfig(queue)), WithBus(rb), WithLedger(l), WithTools(echoTool(nil)))
	if err := qb.Publish(ctx, queue, job(t, RunRequest{Input: "fails", Budget: failingBudget()})); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the failure", func() bool {
		r := runByInput(t, a, "fails")
		return r != nil && r.State == StateFailed
	})
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	a.Close(sctx)
	cancel()
	r := runByInput(t, a, "fails")
	if r.Metadata[MetaIntakeOutcome] != "" {
		t.Fatalf("outcome before the record went out = %q", r.Metadata[MetaIntakeOutcome])
	}
	noRecord(t, dead)

	b := newTestAgent(t, newScript().Model(), WithConfig(intakeConfig(queue)), WithBus(qb), WithLedger(l))
	_, d := nextRecord(t, dead)
	if d.Run != r.ID || d.Outcome != DeadFailed {
		t.Fatalf("record = %+v", d)
	}
	waitUntil(t, "the outcome", func() bool {
		got, _ := b.Get(ctx, r.ID)
		return got.Metadata[MetaIntakeOutcome] == IntakeDeadLettered
	})
	noRecord(t, dead)
	if got, _ := b.Get(ctx, "OLD"); got.Metadata[MetaIntakeOutcome] != "" {
		t.Fatalf("an old run got an outcome: %v", got.Metadata)
	}
}

// The gate admits no more messages than the run slots that no driver and
// no admitted message holds, and returns at the drain.
func TestIntakeGate(t *testing.T) {
	k := &kernel{sem: make(chan struct{}, 2), slotsChanged: make(chan struct{}), drainCh: make(chan struct{})}
	g := intakeGate{k: k}
	ctx := testCtx(t)
	if n, err := g.Admit(ctx, 5); err != nil || n != 2 {
		t.Fatalf("Admit = %d, %v; want 2", n, err)
	}
	g.Unused(1)
	k.addRootDriver(1)
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := g.Admit(short, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Admit with every slot held = %v", err)
	}
	got := make(chan int, 1)
	go func() {
		n, _ := g.Admit(ctx, 3)
		got <- n
	}()
	k.addRootDriver(-1)
	if n := <-got; n != 1 {
		t.Fatalf("Admit after a driver ended = %d, want 1", n)
	}
	g.free(2)
	k.addRootDriver(2)
	go func() {
		_, err := g.Admit(ctx, 1)
		got <- map[bool]int{true: 1, false: 0}[errors.Is(err, ErrClosed)]
	}()
	close(k.drainCh)
	if n := <-got; n != 1 {
		t.Fatal("Admit did not return ErrClosed at the drain")
	}
}

func TestDeadLetterSubject(t *testing.T) {
	off := false
	for _, c := range []struct {
		cfg  BusConfig
		want string
	}{
		{BusConfig{Intake: "fw.jobs.migrator"}, "fw.jobs.migrator.dead"},
		{BusConfig{Intake: "fw.jobs.migrator", DeadLetter: &off}, ""},
		{BusConfig{}, ""},
		{BusConfig{Intake: "fw.jobs.*"}, ""},
		{BusConfig{Intake: "fw.jobs.>"}, ""},
	} {
		if got := c.cfg.DeadLetterSubject(); got != c.want {
			t.Errorf("DeadLetterSubject(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
	if (&Config{}).RunSlots() != defaultMaxRuns || (&Config{MaxRuns: 1}).RunSlots() != 1 {
		t.Error("RunSlots")
	}
}

// failingBudget makes a run fail at its first turn. The final report is
// off, because these tests watch what the intake does with a failed run,
// and the scripted model has no answer for a report call.
func failingBudget() *Budget {
	off := false
	return &Budget{MaxTurns: 1, FinalReport: &off}
}

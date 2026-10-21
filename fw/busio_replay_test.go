package fw

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// replayBus records synchronous publication order, including an accepted
// message whose ACK is lost. MemBus's subscriber goroutines cannot do that.
type replayBus struct {
	Bus
	mu        sync.Mutex
	events    []Event
	subjects  []string
	attempts  int
	fail      bool
	uncertain bool
	hook      func(Event)
}

func (b *replayBus) Publish(ctx context.Context, subject string, ev Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.attempts++
	if b.fail {
		b.mu.Unlock()
		return errors.New("disconnected")
	}
	b.events = append(b.events, ev)
	b.subjects = append(b.subjects, subject)
	uncertain, hook := b.uncertain, b.hook
	b.uncertain, b.hook = false, nil
	b.mu.Unlock()
	if hook != nil {
		hook(ev)
	}
	if uncertain {
		return errors.New("accepted, but ACK lost")
	}
	return nil
}

func (b *replayBus) snapshot() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Event(nil), b.events...)
}

func appendReplayEvent(t *testing.T, l Ledger, id, typ string) Event {
	t.Helper()
	ev, err := NewEvent(id, typ, nil)
	if err != nil {
		t.Fatal(err)
	}
	ev.Source = "fw://test/default/" + id
	ev.Seq, err = l.Append(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func testReplay(t *testing.T, l *FileLedger, bus Bus, types ...string) *egress {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := DefaultConfig()
	cfg.Bus.EgressTypes = types
	a := &Agent{ledger: l, bus: bus, log: quietLogger(), cfg: cfg, k: &kernel{ctx: ctx, atespace: "test", drainCh: make(chan struct{})}}
	e := newEgress(a, "fw.events")
	e.route = "test-route"
	return e
}

func waitReplay(t *testing.T, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for egress")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A finished run is not resumed. Its unpublished final event must therefore
// come from the ledger on boot, without another append or observer callback.
func TestBusEgressReplaysUnpublishedFinalAfterRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	bus := &replayBus{Bus: NewMemBus(), fail: true}
	cfg := DefaultConfig()
	cfg.Bus.Egress, cfg.Bus.EgressTypes = "fw.events", []string{"fw.run.*"}
	a := newTestAgent(t, newScript(endTurn("ok")).Model(), WithConfig(cfg), WithLedger(l), WithBus(bus))
	r, err := a.Start(testCtx(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(testCtx(t), r.ID, func(r *Run) bool { return r.State.Terminal() }); err != nil {
		t.Fatal(err)
	}
	a.Close(testCtx(t))
	var want []Event
	for _, ev := range ledgerEvents(t, l, r.ID) {
		if typeSelected(cfg.Bus.EgressTypes, ev.Type) {
			want = append(want, ev)
		}
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	bus.mu.Lock()
	bus.fail = false
	bus.mu.Unlock()
	a2 := newTestAgent(t, newScript().Model(), WithConfig(cfg), WithLedger(l2), WithBus(bus))
	waitReplay(t, func() bool { return len(bus.snapshot()) == len(want) })
	a2.Close(testCtx(t)) // waits for the cursor save as well as publication
	got := bus.snapshot()
	for i, ev := range want {
		if got[i].ID != ev.ID || got[i].Seq != ev.Seq {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], ev)
		}
	}
	if got[len(got)-1].Type != EvRunFinished {
		t.Fatal("missing final event")
	}
	l3, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	a3 := newTestAgent(t, newScript().Model(), WithConfig(cfg), WithLedger(l3), WithBus(bus))
	time.Sleep(150 * time.Millisecond)
	a3.Close(testCtx(t))
	if n := len(bus.snapshot()); n != len(want) {
		t.Fatalf("acknowledged history replayed again: %d events, want %d", n, len(want))
	}
}

func TestBusEgressUncertainACKReplaysStableIDAfterRestart(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := appendReplayEvent(t, l, "run", EvRunQueued)
	uncertain := appendReplayEvent(t, l, "run", EvRunStarted)
	last := appendReplayEvent(t, l, "run", EvRunFinished)
	bus := &replayBus{Bus: NewMemBus()}
	bus.hook = func(Event) {
		bus.mu.Lock()
		bus.uncertain = true // the second publication is accepted without ACK
		bus.mu.Unlock()
	}
	e := testReplay(t, l, bus)
	if _, err := e.replayRun(context.Background(), "run"); err == nil {
		t.Fatal("expected lost ACK")
	}
	if seq, err := l.LoadEgressCursor(context.Background(), e.route, "run"); err != nil || seq != first.Seq {
		t.Fatalf("cursor = %d, %v; must stop before uncertain seq %d", seq, err, uncertain.Seq)
	}
	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	e2 := testReplay(t, l2, bus)
	if _, err := e2.replayRun(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	got := bus.snapshot()
	if len(got) != 4 || got[0].ID != first.ID || got[1].ID != uncertain.ID || got[2].ID != uncertain.ID || got[3].ID != last.ID {
		t.Fatalf("replayed events = %+v", got)
	}
}

func TestBusEgressReconnectFiltersAndReplayObserverOverlap(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := appendReplayEvent(t, l, "run", EvRunQueued)
	appendReplayEvent(t, l, "run", EvModelRequested)
	bus := &replayBus{Bus: NewMemBus(), fail: true}
	e := testReplay(t, l, bus, "fw.run.*")
	if _, err := e.replayRun(context.Background(), "run"); err == nil {
		t.Fatal("expected disconnected bus")
	}
	if seq, err := l.LoadEgressCursor(context.Background(), e.route, "run"); err != nil || seq != 0 {
		t.Fatalf("disconnected cursor = %d, %v", seq, err)
	}
	final := appendReplayEvent(t, l, "run", EvRunFinished)
	e.observe(final) // queued wake before replay
	var overlap Event
	bus.mu.Lock()
	bus.fail = false
	bus.hook = func(Event) {
		overlap = appendReplayEvent(t, l, "run", EvRunAnnotated)
		e.observe(overlap) // append during replay must not be queued separately
	}
	bus.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := e.replayRun(context.Background(), "run"); err != nil {
			t.Fatal(err)
		}
	}
	got := bus.snapshot()
	if len(got) != 3 || got[0].ID != first.ID || got[1].ID != final.ID || got[2].ID != overlap.ID {
		t.Fatalf("filtered replay/observer overlap = %+v", got)
	}
	if len(e.queue) != 0 {
		t.Fatal("durable observer retained events")
	}
	bus.mu.Lock()
	for _, subject := range bus.subjects {
		if subject != "fw.events.test.default."+got[0].Type && subject != "fw.events.test.default."+final.Type && subject != "fw.events.test.default."+overlap.Type {
			t.Errorf("unexpected subject %q", subject)
		}
	}
	bus.mu.Unlock()
	// A different filter/route owns an independent cursor and replays history.
	e = testReplay(t, l, bus, EvModelRequested)
	e.route = "new-route-with-model-filter"
	if _, err := e.replayRun(context.Background(), "run"); err != nil {
		t.Fatal(err)
	}
	got = bus.snapshot()
	if len(got) != 4 || got[3].Type != EvModelRequested {
		t.Fatalf("changed route did not replay selected history: %+v", got)
	}
}

func TestBusEgressBoundedReplayFairnessAndClose(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Exceed the legacy queue limit without keeping any egress event queue.
	for i := 0; i < egressQueueMax+1; i++ {
		appendReplayEvent(t, l, "large", EvRunStarted)
	}
	short := appendReplayEvent(t, l, "short", EvRunFinished)
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l, bus)
	for _, ev := range ledgerEvents(t, l, "large") {
		e.observe(ev)
	}
	if len(e.queue) != 0 || len(e.wake) != 1 {
		t.Fatalf("unbounded observer: queue=%d wake=%d", len(e.queue), len(e.wake))
	}
	more, err := e.replayRun(context.Background(), "large")
	if err != nil || !more || len(bus.snapshot()) != egressReplayBatch {
		t.Fatalf("batch = %v, %v, publications %d", more, err, len(bus.snapshot()))
	}
	// A loop pass must visit the short run before finishing the large run.
	e.a.k.wg.Add(1)
	go e.replayLoop()
	waitReplay(t, func() bool {
		for _, ev := range bus.snapshot() {
			if ev.ID == short.ID {
				return true
			}
		}
		return false
	})
	close(e.a.k.drainCh)
	e.a.k.wg.Wait()
	got := bus.snapshot()
	for i, ev := range got {
		if ev.ID == short.ID && i > 2*egressReplayBatch {
			t.Fatalf("short run starved behind %d events", i)
		}
	}
	// After the replay closed, as in a shutdown, the observer publishes
	// inline and leaves the cursor, so the next owner sends the event again.
	before := len(got)
	ev := appendReplayEvent(t, l, "short", EvRunAnnotated)
	e.observe(ev)
	if got := bus.snapshot(); len(got) != before+1 || got[before].ID != ev.ID {
		t.Fatalf("the observer did not publish inline after the close: %d events", len(got)-before)
	}
	if seq, err := l.LoadEgressCursor(context.Background(), e.route, "short"); err != nil || seq != short.Seq {
		t.Fatalf("the inline publication moved the cursor: %d, %v", seq, err)
	}
	// A subsequent owner can recover every event without the old queue's cap.
	e2 := testReplay(t, l, bus)
	for {
		more, err := e2.replayRun(context.Background(), "large")
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	seen := make(map[uint64]bool)
	for _, ev := range bus.snapshot() {
		if ev.Subject == "large" {
			seen[ev.Seq] = true
		}
	}
	if len(seen) != egressQueueMax+1 {
		t.Fatalf("backlog lost events: got %d", len(seen))
	}
}

func TestBusEgressRouteCanonicalFilters(t *testing.T) {
	base := egressRoute("prefix", "atespace", []string{"fw.run.*", "fw.tool.*"})
	if got := egressRoute("prefix", "atespace", []string{"fw.tool.*", "fw.run.*", "fw.run.*"}); got != base {
		t.Fatalf("reordered/repeated filters changed route: %q != %q", got, base)
	}
	if egressRoute("prefix", "atespace", nil) != egressRoute("prefix", "atespace", []string{}) {
		t.Fatal("nil and empty filters changed route")
	}
	for _, route := range []string{
		egressRoute("different", "atespace", []string{"fw.run.*", "fw.tool.*"}),
		egressRoute("prefix", "different", []string{"fw.run.*", "fw.tool.*"}),
		egressRoute("prefix", "atespace", []string{"fw.run.*"}),
	} {
		if route == base {
			t.Fatal("changed publication route reused cursor namespace")
		}
	}
}

func TestBusEgressDurableLoopReconnectsWithoutNewAppend(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := appendReplayEvent(t, l, "run", EvRunQueued)
	final := appendReplayEvent(t, l, "run", EvRunFinished)
	bus := &replayBus{Bus: NewMemBus(), fail: true}
	e := testReplay(t, l, bus)
	e.a.k.wg.Add(1)
	go e.replayLoop()
	t.Cleanup(func() {
		close(e.a.k.drainCh)
		e.a.k.wg.Wait()
	})
	time.Sleep(50 * time.Millisecond)
	if len(bus.snapshot()) != 0 {
		t.Fatal("published while disconnected")
	}
	bus.mu.Lock()
	bus.fail = false
	bus.mu.Unlock()
	// There is no observer callback on reconnect; the retry owns progress.
	waitReplay(t, func() bool {
		seq, err := l.LoadEgressCursor(context.Background(), e.route, "run")
		return err == nil && seq == final.Seq
	})
	got := bus.snapshot()
	if len(got) != 2 || got[0].ID != first.ID || got[1].ID != final.ID {
		t.Fatalf("reconnect did not publish persisted history in order: %+v", got)
	}
}

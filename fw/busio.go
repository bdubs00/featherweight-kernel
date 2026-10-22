package fw

// This file connects the kernel to the bus (docs/ARCHITECTURE.md section
// 9.4). Egress publishes ledger events to subjects. The intake, which
// consumes run requests from a queue, is in intake.go.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const egressTimeout = 5 * time.Second

// egressQueueMax is the number of events that the kernel keeps for the bus
// while the bus is out of reach. Beyond it, the oldest event goes.
const egressQueueMax = 10_000

// egressReplayBatch is the most events that one pass publishes for one
// run, so a long run cannot hold back the others.
const egressReplayBatch = 128

// egressRescan is the time between two passes over every run of the
// ledger. The observer names each new event at once. The full pass finds
// a run that no observer of this process named, and tries a run again
// that failed with an error of its own.
const egressRescan = time.Minute

// egress publishes each ledger event to "<egress>.<atespace>.<agent>.<type>".
// A ledger with EgressCursors is the queue itself: the observer only names
// the run of each new event, and one goroutine reads the events after the
// cursor of the run from the ledger, publishes them in order, and saves
// the cursor. An event that the bus did not confirm stays after the
// cursor, so the boot after a crash or a suspend sends it. Another ledger
// keeps the events in an ordered queue in memory, up to egressQueueMax.
// Either way, a ledger append never waits for the bus, and when the bus
// refuses, the goroutine tries again with a pause that doubles up to
// intakeRetryMax. Config.Bus.EgressTypes, when set, limits the types. Each
// entry is a type or a glob such as "fw.run.*". Config.Bus.EgressMaxBytes,
// when above 0, cuts each long string value in the data of the copy that
// goes to the bus (cutStrings); the ledger keeps the event whole.
type egress struct {
	a        *Agent
	prefix   string
	atespace string
	types    []string
	maxBytes int           // Config.Bus.EgressMaxBytes; 0 publishes the data whole
	cursors  EgressCursors // nil: the queue in memory
	route    string        // the name of the cursors of this prefix, atespace, and filter

	// cursor holds the cursor of each run that this process loaded or
	// saved, and broken the runs whose last visit failed with an error of
	// their own. Only the goroutine of the replay touches them.
	cursor map[string]uint64
	broken map[string]bool

	mu      sync.Mutex
	queue   []egressItem
	dropped int
	dirty   []string        // the runs with events that the replay has not read, oldest first
	marked  map[string]bool // the runs in dirty
	closed  bool
	wake    chan struct{}
}

type egressItem struct {
	subject string
	event   Event
}

// egressRefused wraps a refusal of the bus. A pass of the replay stops at
// the first one, because the next run would meet the same bus.
type egressRefused struct{ err error }

func (r egressRefused) Error() string { return r.err.Error() }
func (r egressRefused) Unwrap() error { return r.err }

func (a *Agent) startEgress() {
	prefix := a.cfg.Bus.Egress
	if prefix == "" || a.bus == nil {
		return
	}
	e := newEgress(a, prefix)
	if e.cursors == nil {
		a.log.Warn("bus egress: the ledger keeps no egress cursors, so the events for the bus wait in memory")
	}
	// The observer comes before the first pass of the replay, so an event
	// of the recovery is in that pass or in the next one.
	a.Observe(ObserverFunc(e.observe))
	a.k.wg.Add(1)
	if e.cursors != nil {
		go e.replayLoop()
	} else {
		go e.loop()
	}
}

func newEgress(a *Agent, prefix string) *egress {
	e := &egress{a: a, prefix: prefix, atespace: a.k.atespace, types: a.cfg.Bus.EgressTypes, maxBytes: max(a.cfg.Bus.EgressMaxBytes, 0), wake: make(chan struct{}, 1)}
	e.cursors, _ = a.ledger.(EgressCursors)
	if e.cursors != nil {
		e.route = egressRoute(prefix, e.atespace, e.types)
		e.cursor, e.broken, e.marked = map[string]uint64{}, map[string]bool{}, map[string]bool{}
	}
	return e
}

// egressRoute names the cursors of one prefix, atespace, and filter. The
// order and the repeats of the filter do not change the name. Any other
// change starts new cursors, so the new route sends the history again.
func egressRoute(prefix, atespace string, types []string) string {
	canonical := slices.Clone(types)
	slices.Sort(canonical)
	canonical = slices.Compact(canonical)
	if len(canonical) == 0 {
		canonical = nil
	}
	route, _ := json.Marshal([]any{prefix, atespace, canonical})
	return string(route)
}

func (e *egress) subject(ev Event) string {
	return e.prefix + "." + e.atespace + "." + agentOfSource(ev.Source) + "." + ev.Type
}

func (e *egress) observe(ev Event) {
	if e.cursors != nil {
		e.mark(ev)
		return
	}
	if !typeSelected(e.types, ev.Type) {
		return
	}
	item := egressItem{subject: e.subject(ev), event: ev}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		// The loop is gone, so the last events of a shutdown go out inline.
		if err := e.publish(context.Background(), item); err != nil {
			e.a.log.Warn("bus egress: publish failed", "subject", item.subject, "error", err)
		}
		return
	}
	if len(e.queue) >= egressQueueMax {
		e.queue = e.queue[1:]
		e.dropped++
		if e.dropped == 1 || e.dropped%1000 == 0 {
			e.a.log.Warn("bus egress: the queue is full; the oldest events go", "dropped", e.dropped, "max", egressQueueMax)
		}
	}
	e.queue = append(e.queue, item)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// mark names the run of an event for the replay. After the replay ended,
// at a shutdown, the event goes out inline, and its cursor stays where it
// is, so the next boot sends the event again under the same id.
func (e *egress) mark(ev Event) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		if !typeSelected(e.types, ev.Type) {
			return
		}
		item := egressItem{subject: e.subject(ev), event: ev}
		if err := e.publish(context.Background(), item); err != nil {
			e.a.log.Warn("bus egress: publish failed", "subject", item.subject, "error", err)
		}
		return
	}
	e.markLocked(ev.Subject)
	e.mu.Unlock()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *egress) markLocked(id string) {
	if !e.marked[id] {
		e.marked[id] = true
		e.dirty = append(e.dirty, id)
	}
}

// takeDirty returns the runs that wait for the replay and empties the list.
func (e *egress) takeDirty() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := e.dirty
	e.dirty = nil
	clear(e.marked)
	return ids
}

// requeue puts runs back on the list, ahead of the runs that came since.
func (e *egress) requeue(ids []string) {
	if len(ids) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	later := e.dirty
	e.dirty = nil
	clear(e.marked)
	for _, id := range ids {
		e.markLocked(id)
	}
	for _, id := range later {
		e.markLocked(id)
	}
}

// publish sends one event and counts it. With a bound on the strings, the
// copy of the event that goes to the bus has each longer string value of
// its data cut; the event of the ledger and of the queue stays whole.
func (e *egress) publish(ctx context.Context, item egressItem) error {
	ctx, cancel := context.WithTimeout(ctx, egressTimeout)
	defer cancel()
	ev := item.event
	if e.maxBytes > 0 {
		ev.Data = cutStrings(ev.Data, e.maxBytes)
	}
	if err := e.a.bus.Publish(ctx, item.subject, ev); err != nil {
		return err
	}
	e.a.countBus("out")
	return nil
}

func (e *egress) head() (egressItem, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.queue) == 0 {
		return egressItem{}, false
	}
	return e.queue[0], true
}

func (e *egress) pop() {
	e.mu.Lock()
	e.queue = e.queue[1:]
	e.mu.Unlock()
}

func (e *egress) size() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

// loop drains the queue until the kernel closes. When the kernel starts to
// drain, it sends what is queued while the bus answers, and then the
// observer publishes the events of the shutdown itself.
func (e *egress) loop() {
	k := e.a.k
	defer k.wg.Done()
	delay := time.Second
	failing := false
	finish := func() {
		for {
			item, ok := e.head()
			if !ok {
				break
			}
			if err := e.publish(context.Background(), item); err != nil {
				e.a.log.Warn("bus egress: the bus refused during the shutdown; events are lost", "left", e.size(), "error", err)
				break
			}
			e.pop()
		}
		e.mu.Lock()
		e.closed = true
		e.queue = nil
		e.mu.Unlock()
	}
	for {
		item, ok := e.head()
		if !ok {
			select {
			case <-k.ctx.Done():
				finish()
				return
			case <-k.drainCh:
				finish()
				return
			case <-e.wake:
			}
			continue
		}
		if err := e.publish(context.Background(), item); err != nil {
			if !failing {
				e.a.log.Warn("bus egress: publish failed; the events wait for the bus", "subject", item.subject, "queued", e.size(), "error", err)
				failing = true
			}
			t := time.NewTimer(delay)
			select {
			case <-k.ctx.Done():
				t.Stop()
				finish()
				return
			case <-k.drainCh:
				t.Stop()
				finish()
				return
			case <-t.C:
			}
			delay = min(delay*2, intakeRetryMax)
			continue
		}
		if failing {
			e.a.log.Info("bus egress: publishing again", "queued", e.size())
			failing = false
		}
		delay = time.Second
		e.pop()
	}
}

func (e *egress) stopping() bool {
	select {
	case <-e.a.k.ctx.Done():
		return true
	case <-e.a.k.drainCh:
		return true
	default:
		return false
	}
}

// replayRun publishes the events of one run after its cursor, in order, up
// to egressReplayBatch of them, and saves the cursor past the events that
// the bus confirmed or the filter skipped. It reports whether the run has
// more events than the batch. The runs share no sequence, so the order
// holds within a run and not between runs.
func (e *egress) replayRun(ctx context.Context, id string) (bool, error) {
	from, ok := e.cursor[id]
	if !ok {
		var err error
		if from, err = e.cursors.LoadEgressCursor(ctx, e.route, id); err != nil {
			return false, err
		}
		e.cursor[id] = from
	}
	last, n := from, 0
	var failed error
	for ev, err := range e.a.ledger.Read(ctx, id, from+1) {
		if err != nil {
			failed = err
			break
		}
		if typeSelected(e.types, ev.Type) {
			if err := e.publish(ctx, egressItem{subject: e.subject(ev), event: ev}); err != nil {
				// A refusal can hide a message that the bus took after
				// all. The event stays after the cursor, and the next try
				// sends it again under the same id.
				failed = egressRefused{err}
				break
			}
		}
		last = ev.Seq
		n++
		if n == egressReplayBatch {
			break
		}
	}
	if last != from {
		if err := e.cursors.SaveEgressCursor(ctx, e.route, id, last); err != nil {
			// The cursor on the disk is unknown now. The next visit loads
			// it again, so the events after it go out a second time.
			delete(e.cursor, id)
			return false, err
		}
		e.cursor[id] = last
	}
	return n == egressReplayBatch, failed
}

// pass visits the runs that wait, and every run of the ledger when all is
// set. A run with more events than one batch waits for the next pass. A
// run that fails with an error of its own waits for the next full pass,
// so it cannot slow the others. At a refusal of the bus, or when stop
// reports true, the pass puts back the runs that it did not visit and
// ends. It reports whether a run waits for more, and the refusal of the
// bus or the error of the ledger index.
func (e *egress) pass(ctx context.Context, all bool, stop func() bool) (bool, error) {
	ids := e.takeDirty()
	if all {
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			seen[id] = true
		}
		for id, err := range e.a.ledger.Runs(ctx) {
			if err != nil {
				e.requeue(ids)
				return false, err
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	var again []string
	for i, id := range ids {
		if stop() || ctx.Err() != nil {
			e.requeue(append(again, ids[i:]...))
			return true, ctx.Err()
		}
		more, err := e.replayRun(ctx, id)
		var refused egressRefused
		switch {
		case errors.As(err, &refused) || (err != nil && ctx.Err() != nil):
			e.requeue(append(again, ids[i:]...))
			return false, err
		case err != nil:
			if !e.broken[id] {
				e.a.log.Warn("bus egress: cannot replay a run; its events wait in the ledger", "run", id, "error", err)
				e.broken[id] = true
			}
		default:
			delete(e.broken, id)
			if more {
				again = append(again, id)
			}
		}
	}
	e.requeue(again)
	return len(again) > 0, nil
}

// replayLoop publishes from the ledger until the kernel closes. A wake of
// the observer starts a pass over the runs with new events, and a pass
// over every run comes at the start and then every egressRescan. When the
// bus refuses, the loop waits with a pause that doubles up to
// intakeRetryMax, and a wake does not shorten the pause. When the kernel
// starts to drain, the loop sends what waits while the bus answers, and
// then the observer publishes the events of the shutdown itself.
func (e *egress) replayLoop() {
	k := e.a.k
	defer k.wg.Done()
	delay := time.Second
	failing := false
	full := time.Now() // the time of the next pass over every run
	for {
		if e.stopping() {
			e.finish()
			return
		}
		all := !time.Now().Before(full)
		if all {
			full = time.Now().Add(egressRescan)
		}
		more, err := e.pass(k.ctx, all, e.stopping)
		if err != nil && e.stopping() {
			continue
		}
		if err != nil {
			if !failing {
				e.a.log.Warn("bus egress: publish failed; the events wait in the ledger", "error", err)
				failing = true
			}
			t := time.NewTimer(delay)
			select {
			case <-k.ctx.Done():
			case <-k.drainCh:
			case <-t.C:
			}
			t.Stop()
			delay = min(delay*2, intakeRetryMax)
			continue
		}
		if failing {
			e.a.log.Info("bus egress: publishing again")
			failing = false
		}
		delay = time.Second
		if more {
			continue
		}
		t := time.NewTimer(time.Until(full))
		select {
		case <-k.ctx.Done():
		case <-k.drainCh:
		case <-e.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// finish sends what waits at a shutdown while the bus answers, until the
// kernel cancels its context. It drains the list of runs, closes the
// replay so the observer publishes inline from then on, and drains the
// runs that the observer named in between.
func (e *egress) finish() {
	ctx := e.a.k.ctx
	never := func() bool { return false }
	drain := func() error {
		for {
			more, err := e.pass(ctx, false, never)
			if err != nil || !more {
				return err
			}
		}
	}
	err := drain()
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	if err == nil {
		err = drain()
	}
	if err != nil {
		e.a.log.Warn("bus egress: the shutdown left events in the ledger; the next boot sends them", "error", err)
	}
}

func typeSelected(types []string, typ string) bool {
	if len(types) == 0 {
		return true
	}
	for _, t := range types {
		if globMatch(t, typ) {
			return true
		}
	}
	return false
}

// agentOfSource returns the agent of a source "fw://<atespace>/<agent>/<run>".
func agentOfSource(src string) string {
	rest, ok := strings.CutPrefix(src, "fw://")
	if !ok {
		return "unknown"
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		return "unknown"
	}
	return strings.Join(parts[1:len(parts)-1], "/")
}

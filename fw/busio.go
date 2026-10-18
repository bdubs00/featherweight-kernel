package fw

// This file connects the kernel to the bus (docs/ARCHITECTURE.md sections
// 9.3 and 9.4). Intake consumes run requests from a queue. Egress publishes
// ledger events to subjects.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const egressTimeout = 5 * time.Second

// intakeRetryMax caps the pause between two attempts to start the intake.
const intakeRetryMax = 30 * time.Second

// startIntake consumes Config.Bus.Intake. Each message is a RunRequest in
// JSON. The message id is the idempotency key when the request has none, so
// a redelivered message returns the first run. The handler acknowledges the
// message only when the run reaches a terminal state.
//
// The bus can be out of reach when the process boots. In a sandbox the
// egress policy can take effect seconds after the first packet, and a bus
// server can restart. A consumer that does not start is a warning, not a
// failure: the kernel tries again in the background, with a pause that
// doubles up to intakeRetryMax, until the consumer starts or the kernel
// closes. The process serves its API and its health in the meantime.
func (a *Agent) startIntake() {
	queue := a.cfg.Bus.Intake
	if queue == "" || a.bus == nil {
		return
	}
	if a.tryIntake(queue) {
		return
	}
	a.k.wg.Add(1)
	go func() {
		defer a.k.wg.Done()
		delay := time.Second
		for {
			t := time.NewTimer(delay)
			select {
			case <-a.k.ctx.Done():
				t.Stop()
				return
			case <-a.k.drainCh:
				t.Stop()
				return
			case <-t.C:
			}
			if a.tryIntake(queue) {
				return
			}
			delay = min(delay*2, intakeRetryMax)
		}
	}()
}

// tryIntake starts the consumer once. It returns true when the consumer
// runs, or when the kernel is closing and a retry has no point.
func (a *Agent) tryIntake(queue string) bool {
	k := a.k
	stop, err := a.bus.Consume(k.ctx, queue, a.handleIntake)
	if err != nil {
		if k.ctx.Err() != nil {
			return true
		}
		a.log.Warn("bus intake not started yet; retrying in the background", "queue", queue, "error", err)
		return false
	}
	k.lifeMu.Lock()
	if k.draining {
		k.lifeMu.Unlock()
		stop()
		return true
	}
	k.intakeStop = stop
	k.lifeMu.Unlock()
	a.log.Info("bus intake started", "queue", queue)
	return true
}

func (a *Agent) handleIntake(e Event) error {
	a.countBus("in")
	var req RunRequest
	if err := json.Unmarshal(e.Data, &req); err != nil {
		a.log.Warn("bus intake: drop a message that is not a run request", "id", e.ID, "error", err)
		return nil
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = e.ID
	}
	req.Origin = OriginBus
	r, err := a.start(a.k.ctx, req)
	if err != nil {
		if errors.Is(err, ErrClosed) {
			return err
		}
		// A request that can never start would come back forever.
		a.log.Warn("bus intake: drop a run request that cannot start", "id", e.ID, "error", err)
		return nil
	}
	_, err = a.WaitFor(a.k.ctx, r.ID, func(r *Run) bool { return r.State.Terminal() })
	return err
}

// egressQueueMax is the number of events that the kernel keeps for the bus
// while the bus is out of reach. Beyond it, the oldest event goes.
const egressQueueMax = 10_000

// egress publishes each ledger event to "<egress>.<atespace>.<agent>.<type>"
// through an ordered queue. The observer only appends, so a ledger append
// never waits for the bus. One goroutine drains the queue in order and,
// when the bus refuses, tries again with a pause that doubles up to
// intakeRetryMax. A boot without the bus, as under ax before the sandbox
// opens its egress, loses no event this way. Config.Bus.EgressTypes, when
// set, limits the types. Each entry is a type or a glob such as "fw.run.*".
type egress struct {
	a        *Agent
	prefix   string
	atespace string
	types    []string

	mu      sync.Mutex
	queue   []egressItem
	dropped int
	closed  bool
	wake    chan struct{}
}

type egressItem struct {
	subject string
	event   Event
}

func (a *Agent) startEgress() {
	prefix := a.cfg.Bus.Egress
	if prefix == "" || a.bus == nil {
		return
	}
	e := &egress{a: a, prefix: prefix, atespace: a.k.atespace, types: a.cfg.Bus.EgressTypes, wake: make(chan struct{}, 1)}
	a.Observe(ObserverFunc(e.observe))
	a.k.wg.Add(1)
	go e.loop()
}

func (e *egress) observe(ev Event) {
	if !typeSelected(e.types, ev.Type) {
		return
	}
	item := egressItem{subject: e.prefix + "." + e.atespace + "." + agentOfSource(ev.Source) + "." + ev.Type, event: ev}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		// The loop is gone, so the last events of a shutdown go out inline.
		if err := e.publish(item); err != nil {
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

// publish sends one event and counts it.
func (e *egress) publish(item egressItem) error {
	ctx, cancel := context.WithTimeout(context.Background(), egressTimeout)
	defer cancel()
	if err := e.a.bus.Publish(ctx, item.subject, item.event); err != nil {
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
			if err := e.publish(item); err != nil {
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
		if err := e.publish(item); err != nil {
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

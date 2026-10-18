package fw

// This file connects the kernel to the bus (docs/ARCHITECTURE.md sections
// 9.3 and 9.4). Intake consumes run requests from a queue. Egress publishes
// ledger events to subjects.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// startEgress adds the observer that publishes each ledger event to
// "<egress>.<atespace>.<agent>.<type>". Config.Bus.EgressTypes, when set,
// limits the types. Each entry is a type or a glob such as "fw.run.*".
func (a *Agent) startEgress() {
	prefix := a.cfg.Bus.Egress
	if prefix == "" || a.bus == nil {
		return
	}
	types := a.cfg.Bus.EgressTypes
	atespace := a.k.atespace
	a.Observe(ObserverFunc(func(e Event) {
		if !typeSelected(types, e.Type) {
			return
		}
		subject := prefix + "." + atespace + "." + agentOfSource(e.Source) + "." + e.Type
		ctx, cancel := context.WithTimeout(context.Background(), egressTimeout)
		defer cancel()
		if err := a.bus.Publish(ctx, subject, e); err != nil {
			a.log.Warn("bus egress: publish failed", "subject", subject, "error", err)
			return
		}
		a.countBus("out")
	}))
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

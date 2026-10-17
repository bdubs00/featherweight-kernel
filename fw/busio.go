package fw

// This file connects the kernel to the bus (docs/ARCHITECTURE.md sections
// 9.3 and 9.4). Intake consumes run requests from a queue. Egress publishes
// ledger events to subjects.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const egressTimeout = 5 * time.Second

// startIntake consumes Config.Bus.Intake. Each message is a RunRequest in
// JSON. The message id is the idempotency key when the request has none, so
// a redelivered message returns the first run. The handler acknowledges the
// message only when the run reaches a terminal state.
func (a *Agent) startIntake() error {
	queue := a.cfg.Bus.Intake
	if queue == "" || a.bus == nil {
		return nil
	}
	stop, err := a.bus.Consume(a.k.ctx, queue, a.handleIntake)
	if err != nil {
		return fmt.Errorf("fw: consume %s: %w", queue, err)
	}
	a.k.intakeStop = stop
	a.log.Info("bus intake started", "queue", queue)
	return nil
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

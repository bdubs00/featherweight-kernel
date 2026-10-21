package fw

import (
	"context"
	"iter"
	"time"
)

// Ledger is the append-only event log. It is the only state.
//
// Append sets Seq and Source, stores the event durably, and returns the
// sequence number. Read replays a run from a sequence number. Watch replays
// from a sequence number and then follows new events until ctx ends. Runs
// lists the ids of every run the ledger holds, oldest first.
type Ledger interface {
	Append(ctx context.Context, e Event) (uint64, error)
	Read(ctx context.Context, runID string, from uint64) iter.Seq2[Event, error]
	Watch(ctx context.Context, runID string, from uint64) (<-chan Event, error)
	Runs(ctx context.Context) iter.Seq2[string, error]
}

// EgressCursors is an optional capability of a Ledger. It makes the bus
// egress survive a restart (docs/ARCHITECTURE.md section 9.4). A cursor is
// the last sequence number of a run that the egress of one route handled:
// the bus confirmed the event, or the filter skipped it. The route is an
// opaque name for the prefix, the atespace, and the filter. A missing
// cursor is zero. A load returns zero when the cursor no longer matches
// the ledger, for example after a recovery cut the run. A save makes the
// events up to seq durable before the cursor, and reports success only
// when the cursor is durable. A failed save can make the egress send
// events again, but it must never skip an event that the bus did not
// confirm. The egress of a ledger without this capability keeps the
// events in memory only.
type EgressCursors interface {
	LoadEgressCursor(ctx context.Context, route, runID string) (uint64, error)
	SaveEgressCursor(ctx context.Context, route, runID string, seq uint64) error
}

// Observer receives every event once, in order, after the ledger stored it.
// An observer must not block for long and must not write to the ledger.
type Observer interface {
	Observe(e Event)
}

// StoreObserver is an Observer that also measures the stores of the agent.
// The kernel calls ObserveLedgerAppend after each append to the ledger, and
// CountBusMessage for each message that the bus intake receives ("in") or
// the bus egress publishes ("out"). driver is ledger.driver or bus.driver
// of the configuration.
type StoreObserver interface {
	ObserveLedgerAppend(driver string, d time.Duration)
	CountBusMessage(driver, direction string)
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(e Event)

// Observe implements Observer.
func (f ObserverFunc) Observe(e Event) { f(e) }

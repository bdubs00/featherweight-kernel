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

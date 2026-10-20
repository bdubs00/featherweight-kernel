package fw

import "context"

// Bus moves events between processes.
//
// Publish sends an event to a subject. Subscribe is fan-out: every subscriber
// of a subject receives every event. Consume is a durable queue with
// at-least-once delivery: one consumer of a queue receives each event, and the
// handler returns nil to acknowledge or an error to request redelivery. Both
// return a stop function.
type Bus interface {
	Publish(ctx context.Context, subject string, e Event) error
	Subscribe(ctx context.Context, subject string, h func(Event) error) (stop func(), err error)
	Consume(ctx context.Context, queue string, h func(Event) error) (stop func(), err error)
}

// GatedBus is a Bus whose queue consumer asks a Gate before it takes
// messages, so a process takes only the work that it can start now, and
// the other consumers of the queue get the rest. The bus intake of the
// kernel uses ConsumeGated when the bus has it, and Consume when not
// (docs/ARCHITECTURE.md section 9.3). The nats and redis drivers implement
// it.
//
// ConsumeGated is Consume with a gate. Before each read of the queue, the
// consumer calls Admit with the number of messages that it can handle at
// that moment, reads at most the admitted number, and gives back the
// admissions that it got no message for with Unused. It hands no message
// to the handler without an admission, a message that it delivers again
// included. A message that it drops without the handler, such as one that
// does not decode, gives its admission back with Unused too. When Admit
// returns an error, the consumer reads no more messages until stop. The
// owner of the gate frees the admission of a message when the handler of
// the message returns.
type GatedBus interface {
	Bus
	ConsumeGated(ctx context.Context, queue string, g Gate, h func(Event) error) (stop func(), err error)
}

// Gate admits the messages of a queue consumer (see GatedBus).
type Gate interface {
	// Admit waits until the handler has room for at least one more
	// message, and admits up to max of them. It returns the number that it
	// admitted, or an error when ctx ends or the owner of the gate closes.
	Admit(ctx context.Context, max int) (int, error)
	// Unused gives back n admissions that the consumer got no message for.
	Unused(n int)
}

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

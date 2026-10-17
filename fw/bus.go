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

// Retainer is a Bus that keeps the last event of each subject.
//
// Retain replaces the kept event of a subject and delivers the event to
// every watcher. A retained event never enters the stream of Publish, so a
// heartbeat does not fill the bus. Watch first delivers the kept event of
// each subject that matches the pattern, then each new one, until stop.
// The pattern is a subject with the wildcards of SubjectMatch. The handler
// runs on one goroutine per watch. The fleet of the workbench keeps the
// records of its members this way.
type Retainer interface {
	Retain(ctx context.Context, subject string, e Event) error
	Watch(ctx context.Context, pattern string, h func(Event) error) (stop func(), err error)
}

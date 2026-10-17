package nats

// This file implements fw.Retainer with a JetStream key-value bucket. The
// key of an entry is the subject, and its value is the event JSON. The
// bucket keeps one value per key, so a heartbeat replaces the last one and
// never fills the stream of Publish.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-kernel/fw"
)

const defaultRetainBucket = "FW_RETAINED"

var (
	validBucket = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	validKey    = regexp.MustCompile(`^[-/_=.a-zA-Z0-9]+$`)
)

var _ fw.Retainer = (*Bus)(nil)

// Retain implements fw.Retainer. It puts the event in the bucket under the
// subject. The bucket keeps the newest event of each subject, and every
// watcher of a matching pattern receives it. Retain publishes nothing on
// the subject itself, so neither a subscriber nor the stream sees the event.
// A subject is a valid key: letters, digits, "-", "_", "=", "/", and "."
// between tokens.
func (b *Bus) Retain(ctx context.Context, subject string, e fw.Event) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	if !validKey.MatchString(subject) {
		return fmt.Errorf("nats: cannot retain %q: a key holds only letters, digits, and the characters - _ = / .", subject)
	}
	if b.isClosed() {
		return errClosed
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("nats: encode event: %w", err)
	}
	kv, err := b.retained(ctx)
	if err != nil {
		return err
	}
	if _, err := kv.Put(ctx, subject, data); err != nil {
		return fmt.Errorf("nats: retain %s: %w", subject, err)
	}
	return nil
}

// Watch implements fw.Retainer with a watcher on the bucket. The handler
// first receives the newest event of each subject that matches the pattern,
// then each new one, in order, on one goroutine. A deleted key is skipped.
//
// The stop function stops the watcher and waits for the handler in flight,
// so no handler call starts after it returns. Do not call it from inside
// the handler. Close stops every watch.
func (b *Bus) Watch(ctx context.Context, pattern string, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("nats: watch without a handler")
	}
	if pattern == "" {
		return nil, errors.New("nats: watch without a pattern")
	}
	if b.isClosed() {
		return nil, errClosed
	}
	kv, err := b.retained(ctx)
	if err != nil {
		return nil, err
	}
	// The ordered consumer behind the watcher keeps its context to create
	// a consumer again after a reset, so it gets one that the caller cannot
	// cancel. The stop function ends the watch.
	kw, err := kv.Watch(context.WithoutCancel(ctx), pattern, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, fmt.Errorf("nats: watch %s: %w", pattern, err)
	}
	w := &watch{b: b, pattern: pattern, kw: kw, h: h, quit: make(chan struct{}), done: make(chan struct{})}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = kw.Stop()
		return nil, errClosed
	}
	if b.watches == nil {
		b.watches = map[*watch]struct{}{}
	}
	b.watches[w] = struct{}{}
	b.mu.Unlock()
	go w.run()
	return w.stop, nil
}

// retained returns the bucket. It finds the bucket on the first call and
// creates it when it does not exist. When another process creates it at
// the same time, the call finds the bucket of that process.
func (b *Bus) retained(ctx context.Context) (jetstream.KeyValue, error) {
	b.retainMu.Lock()
	defer b.retainMu.Unlock()
	if b.kv != nil {
		return b.kv, nil
	}
	kv, err := b.js.KeyValue(ctx, b.retainBucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = b.js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      b.retainBucket,
			Description: "fw: the last event of each retained subject",
			History:     1,
		})
		if errors.Is(err, jetstream.ErrBucketExists) {
			kv, err = b.js.KeyValue(ctx, b.retainBucket)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("nats: retain bucket %s: %w", b.retainBucket, err)
	}
	b.kv = kv
	return kv, nil
}

// watch is one Watch: one goroutine that reads the updates of a watcher.
type watch struct {
	b       *Bus
	pattern string
	kw      jetstream.KeyWatcher
	h       func(fw.Event) error

	stopped atomic.Bool
	once    sync.Once
	quit    chan struct{}
	done    chan struct{}
}

func (w *watch) run() {
	defer close(w.done)
	updates := w.kw.Updates()
	for {
		select {
		case <-w.quit:
			// The watcher can still hold a message. Drain the channel so
			// that the client never blocks on it.
			go func() {
				for range updates {
				}
			}()
			return
		case entry, ok := <-updates:
			if !ok {
				return
			}
			if entry == nil || w.stopped.Load() {
				continue // the end of the initial values
			}
			var e fw.Event
			if err := json.Unmarshal(entry.Value(), &e); err != nil {
				w.b.logger().Warn("bus dropped a retained event that does not decode", "subject", entry.Key(), "error", err)
				continue
			}
			if err := w.h(e); err != nil {
				w.b.logger().Warn("bus watcher failed", "subject", entry.Key(), "id", e.ID, "type", e.Type, "error", err)
			}
		}
	}
}

// stop ends the watch and waits for the handler in flight.
func (w *watch) stop() {
	w.once.Do(func() {
		w.stopped.Store(true)
		close(w.quit)
		_ = w.kw.Stop()
		<-w.done
		w.b.mu.Lock()
		delete(w.b.watches, w)
		w.b.mu.Unlock()
	})
}

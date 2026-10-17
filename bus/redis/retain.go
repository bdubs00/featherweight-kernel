package redis

// This file implements fw.Retainer with one Redis hash. The field of an
// entry is the subject, and its value is the event JSON. Retain also
// publishes the event on the channel "<retainKey>:<subject>", which only a
// watcher hears. The stream of a subject never holds a retained event.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/redis/go-redis/v9"

	"github.com/bdubs00/featherweight-kernel/fw"
)

const (
	defaultRetainKey = "fw:retained"
	retainScanCount  = 100
)

var _ fw.Retainer = (*Bus)(nil)

// Retain implements fw.Retainer. In one pipeline, it sets the field of the
// subject in the hash to the event and publishes the event on the retain
// channel of the subject. A plain subscriber of the subject and the stream
// of the subject never see it.
func (b *Bus) Retain(ctx context.Context, subject string, e fw.Event) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	if b.isClosed() {
		return errClosed
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("redis: encode event: %w", err)
	}
	_, err = b.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.HSet(ctx, b.retainKey, subject, data)
		p.Publish(ctx, b.retainChannel(subject), data)
		return nil
	})
	if err != nil {
		return fmt.Errorf("redis: retain %s: %w", subject, err)
	}
	return nil
}

// Watch implements fw.Retainer. It subscribes to the retain channels that
// match the pattern first, then reads the hash. The handler receives the
// kept events sorted by subject, then the live events in order, on one
// goroutine. An event that Retain sends while Watch reads the hash can
// arrive twice: once from the hash and once from the channel.
//
// After stop returns, no new handler call starts. Close stops every watch.
func (b *Bus) Watch(ctx context.Context, pattern string, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("redis: watch without a handler")
	}
	if pattern == "" {
		return nil, errors.New("redis: watch without a pattern")
	}
	if b.isClosed() {
		return nil, errClosed
	}
	// A Redis "*" also matches dots, so the pattern is wider than the
	// subject pattern, and each message is checked with fw.SubjectMatch.
	ps := b.rdb.PSubscribe(ctx, escapeGlob(b.retainKey)+":"+globPattern(pattern))
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("redis: watch %s: %w", pattern, err)
	}
	// The channel buffers the live events while the hash is read.
	ch := ps.Channel(redis.WithChannelSize(subBuffer))
	kept, err := b.kept(ctx, pattern)
	if err != nil {
		_ = ps.Close()
		return nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = ps.Close()
		return nil, errClosed
	}
	b.subs[ps] = struct{}{}
	b.mu.Unlock()

	var stopped atomic.Bool
	deliver := func(subject string, data []byte) {
		if stopped.Load() {
			return
		}
		var e fw.Event
		if err := json.Unmarshal(data, &e); err != nil {
			b.logger().Warn("bus dropped a retained event that does not decode", "subject", subject, "error", err)
			return
		}
		if err := h(e); err != nil {
			b.logger().Warn("bus watcher failed", "subject", subject, "id", e.ID, "type", e.Type, "error", err)
		}
	}
	prefix := b.retainKey + ":"
	go func() {
		for _, k := range kept {
			deliver(k.subject, k.data)
		}
		for m := range ch {
			subject, ok := strings.CutPrefix(m.Channel, prefix)
			if !ok || !fw.SubjectMatch(pattern, subject) {
				continue
			}
			deliver(subject, []byte(m.Payload))
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			stopped.Store(true)
			b.mu.Lock()
			delete(b.subs, ps)
			b.mu.Unlock()
			_ = ps.Close()
		})
	}, nil
}

// keptEvent is one field of the hash.
type keptEvent struct {
	subject string
	data    []byte
}

// kept reads the fields of the hash that match the pattern, sorted by
// subject. HSCAN can return a field twice, so a map keeps one of each.
func (b *Bus) kept(ctx context.Context, pattern string) ([]keptEvent, error) {
	found := map[string]string{}
	var cursor uint64
	for {
		kv, next, err := b.rdb.HScan(ctx, b.retainKey, cursor, globPattern(pattern), retainScanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: read %s: %w", b.retainKey, err)
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if fw.SubjectMatch(pattern, kv[i]) {
				found[kv[i]] = kv[i+1]
			}
		}
		if cursor = next; cursor == 0 {
			break
		}
	}
	out := make([]keptEvent, 0, len(found))
	for subject, data := range found {
		out = append(out, keptEvent{subject: subject, data: []byte(data)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].subject < out[j].subject })
	return out, nil
}

// retainChannel is the channel on which Retain announces a subject.
func (b *Bus) retainChannel(subject string) string { return b.retainKey + ":" + subject }

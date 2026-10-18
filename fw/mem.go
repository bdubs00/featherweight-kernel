package fw

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
)

func init() {
	RegisterLedger("mem", func(context.Context, *Config) (Ledger, error) { return NewMemLedger(), nil })
	RegisterBus("mem", func(context.Context, *Config) (Bus, error) { return NewMemBus(), nil })
	RegisterSecrets("env", func(context.Context, *Config) (Secrets, error) { return EnvSecrets{}, nil })
}

// MemLedger keeps events in memory. It is for tests and for runs that need
// no durability.
type MemLedger struct {
	mu       sync.RWMutex
	runs     map[string][]Event
	order    []string
	watchers map[string][]*memWatcher
}

type memWatcher struct {
	mu      sync.Mutex
	pending []Event
	notify  chan struct{}
	ch      chan Event
	done    chan struct{}
}

// NewMemLedger returns an empty in-memory ledger.
func NewMemLedger() *MemLedger {
	return &MemLedger{runs: map[string][]Event{}, watchers: map[string][]*memWatcher{}}
}

// Append implements Ledger.
func (l *MemLedger) Append(_ context.Context, e Event) (uint64, error) {
	if e.Subject == "" {
		return 0, fmt.Errorf("fw: event without subject")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	evs, ok := l.runs[e.Subject]
	if !ok {
		l.order = append(l.order, e.Subject)
	}
	e.Seq = uint64(len(evs) + 1)
	if e.Source == "" {
		e.Source = "fw://local/" + e.Subject
	}
	l.runs[e.Subject] = append(evs, e)
	for _, w := range l.watchers[e.Subject] {
		w.mu.Lock()
		w.pending = append(w.pending, e)
		w.mu.Unlock()
		select {
		case w.notify <- struct{}{}:
		default:
		}
	}
	return e.Seq, nil
}

// Read implements Ledger.
func (l *MemLedger) Read(_ context.Context, runID string, from uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		l.mu.RLock()
		evs := append([]Event(nil), l.runs[runID]...)
		l.mu.RUnlock()
		for _, e := range evs {
			if e.Seq < from {
				continue
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

// Watch implements Ledger.
func (l *MemLedger) Watch(ctx context.Context, runID string, from uint64) (<-chan Event, error) {
	w := &memWatcher{notify: make(chan struct{}, 1), ch: make(chan Event, 64), done: make(chan struct{})}
	l.mu.Lock()
	replay := append([]Event(nil), l.runs[runID]...)
	l.watchers[runID] = append(l.watchers[runID], w)
	l.mu.Unlock()
	go func() {
		defer close(w.ch)
		defer func() {
			l.mu.Lock()
			ws := l.watchers[runID]
			for i, x := range ws {
				if x == w {
					l.watchers[runID] = append(ws[:i], ws[i+1:]...)
					break
				}
			}
			l.mu.Unlock()
		}()
		last := uint64(0)
		for _, e := range replay {
			if e.Seq < from {
				continue
			}
			select {
			case w.ch <- e:
				last = e.Seq
			case <-ctx.Done():
				return
			}
		}
		for {
			w.mu.Lock()
			batch := w.pending
			w.pending = nil
			w.mu.Unlock()
			for _, e := range batch {
				if e.Seq <= last {
					continue
				}
				select {
				case w.ch <- e:
					last = e.Seq
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-w.notify:
			case <-ctx.Done():
				return
			}
		}
	}()
	return w.ch, nil
}

// Runs implements Ledger.
func (l *MemLedger) Runs(_ context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		l.mu.RLock()
		ids := append([]string(nil), l.order...)
		l.mu.RUnlock()
		for _, id := range ids {
			if !yield(id, nil) {
				return
			}
		}
	}
}

// MemBus delivers events inside one process.
type MemBus struct {
	mu   sync.RWMutex
	subs map[string][]*memSub
	cons map[string][]*memSub
	rr   map[string]int
}

type memSub struct {
	pattern string
	h       func(Event) error
	closed  bool
}

// NewMemBus returns an in-process bus.
func NewMemBus() *MemBus {
	return &MemBus{
		subs: map[string][]*memSub{},
		cons: map[string][]*memSub{},
		rr:   map[string]int{},
	}
}

// Publish implements Bus.
func (b *MemBus) Publish(_ context.Context, subject string, e Event) error {
	b.mu.RLock()
	var targets []*memSub
	for pat, ss := range b.subs {
		if SubjectMatch(pat, subject) {
			for _, s := range ss {
				if !s.closed {
					targets = append(targets, s)
				}
			}
		}
	}
	var queued []*memSub
	for pat, cs := range b.cons {
		if !SubjectMatch(pat, subject) {
			continue
		}
		live := cs[:0:0]
		for _, c := range cs {
			if !c.closed {
				live = append(live, c)
			}
		}
		if len(live) == 0 {
			continue
		}
		b.mu.RUnlock()
		b.mu.Lock()
		i := b.rr[pat] % len(live)
		b.rr[pat] = i + 1
		b.mu.Unlock()
		b.mu.RLock()
		queued = append(queued, live[i])
	}
	b.mu.RUnlock()
	for _, s := range targets {
		go func(s *memSub) { _ = s.h(e) }(s)
	}
	for _, c := range queued {
		go func(c *memSub) {
			for attempt := 0; attempt < 3; attempt++ {
				if err := c.h(e); err == nil {
					return
				}
			}
		}(c)
	}
	return nil
}

// Subscribe implements Bus.
func (b *MemBus) Subscribe(_ context.Context, subject string, h func(Event) error) (func(), error) {
	s := &memSub{pattern: subject, h: h}
	b.mu.Lock()
	b.subs[subject] = append(b.subs[subject], s)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		s.closed = true
		b.mu.Unlock()
	}, nil
}

// Consume implements Bus.
func (b *MemBus) Consume(_ context.Context, queue string, h func(Event) error) (func(), error) {
	s := &memSub{pattern: queue, h: h}
	b.mu.Lock()
	b.cons[queue] = append(b.cons[queue], s)
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		s.closed = true
		b.mu.Unlock()
	}, nil
}

// SubjectMatch matches NATS-style subjects: tokens split by ".", "*" matches
// one token, and ">" matches the rest.
func SubjectMatch(pattern, subject string) bool {
	if pattern == subject {
		return true
	}
	pt := strings.Split(pattern, ".")
	st := strings.Split(subject, ".")
	for i, p := range pt {
		if p == ">" {
			return i < len(st)
		}
		if i >= len(st) {
			return false
		}
		if p != "*" && p != st[i] {
			return false
		}
	}
	return len(pt) == len(st)
}

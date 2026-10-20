package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// testGate admits as many messages as it has rooms. Unused puts rooms
// back. The handlers of the tests free none.
type testGate struct {
	mu      sync.Mutex
	rooms   int
	unused  int
	changed chan struct{}
}

func newTestGate(rooms int) *testGate {
	return &testGate{rooms: rooms, changed: make(chan struct{})}
}

func (g *testGate) Admit(ctx context.Context, max int) (int, error) {
	for {
		g.mu.Lock()
		if g.rooms > 0 {
			n := min(g.rooms, max)
			g.rooms -= n
			g.mu.Unlock()
			return n, nil
		}
		ch := g.changed
		g.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (g *testGate) Unused(n int) {
	g.mu.Lock()
	g.unused += n
	g.mu.Unlock()
	g.add(n)
}

func (g *testGate) add(n int) {
	g.mu.Lock()
	g.rooms += n
	close(g.changed)
	g.changed = make(chan struct{})
	g.mu.Unlock()
}

func (g *testGate) unusedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.unused
}

// The gated consumer reads only the messages that the gate admits, and
// gives back an admission that a read did not fill. The claim loop also
// waits for an admission before it takes the message of a dead consumer.
// Stop ends a wait in Admit.
func TestConsumeGated(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, map[string]string{"concurrency": "4"})
	ctx := context.Background()
	queue := root + ".jobs.g"
	for range 3 {
		publish(t, b, queue, event(t, fw.EvRunQueued))
	}
	g := newTestGate(1)
	got := make(chan fw.Event, 4)
	stop, err := b.ConsumeGated(ctx, queue, g, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	recv(t, got)
	none(t, got, 300*time.Millisecond)
	g.add(2)
	recv(t, got)
	recv(t, got)
	eventually(t, "all acks", func() bool { return pendingCount(t, b, queue) == 0 })
	g.add(1)
	eventually(t, "an unused admission", func() bool { return g.unusedCount() > 0 })
	stop()

	// The message of a dead consumer waits for an admission.
	dead := root + ".jobs.dead"
	if err := b.ensureGroup(ctx, dead); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvRunQueued)
	publish(t, b, dead, e)
	if err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: b.group, Consumer: "dead-1", Streams: []string{dead, ">"}, Count: 1,
	}).Err(); err != nil {
		t.Fatal(err)
	}
	closed := newTestGate(0)
	stop2, err := b.ConsumeGated(ctx, dead, closed, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	none(t, got, 3*b.claimIdle)
	closed.add(1)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
	done := make(chan struct{})
	go func() {
		stop2()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stop waits for a gate that never admits")
	}
}

func TestConcurrencyDefaultsToTheRunSlots(t *testing.T) {
	url, _ := serverURL(t)
	for maxRuns, want := range map[int]int{0: 16, 1: 1, 3: 3} {
		b, err := Open(context.Background(), &fw.Config{MaxRuns: maxRuns, Bus: fw.BusConfig{Driver: "redis", URL: url}})
		if err != nil {
			t.Fatal(err)
		}
		if b.concurrency != want {
			t.Errorf("maxRuns %d: concurrency = %d, want %d", maxRuns, b.concurrency, want)
		}
		_ = b.Close()
	}
}

// A message that does not decode never reaches the handler, so the
// consumer gives its admission back: the next message gets through a gate
// with one room.
func TestConsumeGatedDropGivesTheAdmissionBack(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	ctx := context.Background()
	queue := root + ".jobs.bad"
	g := newTestGate(1)
	got := make(chan fw.Event, 2)
	stop, err := b.ConsumeGated(ctx, queue, g, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := b.rdb.XAdd(ctx, &redis.XAddArgs{Stream: queue, Values: []any{fieldEvent, "not json"}}).Err(); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

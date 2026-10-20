package nats

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

// The gated consumer takes only the messages that the gate admits, and
// the rest wait on the stream for the other consumers of the queue. It
// gives back an admission that a fetch did not fill, and stop ends a wait
// in Admit.
func TestConsumeGated(t *testing.T) {
	b, root := testBus(t, serverURL(t), map[string]string{"concurrency": "4"})
	queue := root + ".jobs.g"
	for range 3 {
		publish(t, b, queue, event(t, fw.EvRunQueued))
	}
	g := newTestGate(1)
	got := make(chan fw.Event, 4)
	stop, err := b.ConsumeGated(context.Background(), queue, g, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	recv(t, got)
	none(t, got, 300*time.Millisecond)
	eventually(t, "the ack of the first message", func() bool { return consumerInfo(t, b, queue).NumAckPending == 0 })
	if n := consumerInfo(t, b, queue).NumPending; n != 2 {
		t.Fatalf("messages on the stream = %d, want 2", n)
	}
	g.add(2)
	recv(t, got)
	recv(t, got)
	eventually(t, "all acks", settled(t, b, queue))
	g.add(1)
	eventually(t, "an unused admission", func() bool { return g.unusedCount() > 0 })
	stop()

	// A consumer that waits in Admit stops at once.
	closed := newTestGate(0)
	stop2, err := b.ConsumeGated(context.Background(), queue, closed, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	publish(t, b, queue, event(t, fw.EvRunQueued))
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
	none(t, got, 100*time.Millisecond)
}

func TestConcurrencyDefaultsToTheRunSlots(t *testing.T) {
	url := serverURL(t)
	for maxRuns, want := range map[int]int{0: 16, 1: 1, 3: 3} {
		root := "t" + strings.ToLower(fw.NewID())
		b, err := Open(context.Background(), &fw.Config{MaxRuns: maxRuns, Bus: fw.BusConfig{Driver: "nats", URL: url,
			Options: map[string]string{"stream": strings.ToUpper(root), "subjects": root + ".>"}}})
		if err != nil {
			t.Fatal(err)
		}
		if b.concurrency != want {
			t.Errorf("maxRuns %d: concurrency = %d, want %d", maxRuns, b.concurrency, want)
		}
		_ = b.js.DeleteStream(context.Background(), b.stream)
		_ = b.Close()
	}
}

// A message that does not decode never reaches the handler, so the
// consumer gives its admission back: the next message gets through a gate
// with one room.
func TestConsumeGatedDropGivesTheAdmissionBack(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	queue := root + ".jobs.bad"
	g := newTestGate(1)
	got := make(chan fw.Event, 2)
	stop, err := b.ConsumeGated(context.Background(), queue, g, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if _, err := b.js.Publish(context.Background(), queue, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

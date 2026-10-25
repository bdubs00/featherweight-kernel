//go:build unix

package env

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// downBus refuses every publish while down is true, as a NATS bus does
// before its first connection.
type downBus struct {
	fw.Bus
	down    atomic.Bool
	refused atomic.Int32
	mu      sync.Mutex
	sent    []string // the types of the events that the bus took, in order
}

func (b *downBus) Publish(ctx context.Context, subject string, e fw.Event) error {
	if b.down.Load() {
		b.refused.Add(1)
		return errors.New("nats: publish fw.environment: nats: not connected yet")
	}
	b.mu.Lock()
	b.sent = append(b.sent, e.Type)
	b.mu.Unlock()
	return b.Bus.Publish(ctx, subject, e)
}

func (b *downBus) taken() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sent...)
}

func newDownFixture(t *testing.T, block string) (*fixture, *downBus) {
	f := newFixture(t, block)
	b := &downBus{}
	b.down.Store(true)
	f.wrap = func(inner fw.Bus) fw.Bus {
		b.Bus = inner
		return b
	}
	return f, b
}

// A golden boot has no network, so the bus refuses the start event. The
// service keeps it and sends the state once the bus takes events, for
// the life of the process.
func TestStartEventWaitsForTheBus(t *testing.T) {
	f, b := newDownFixture(t, fullBlock)
	a := f.agent()
	// An earlier process installed the environment, and the bus refused
	// its event too.
	if _, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	c, err := Start(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for b.refused.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.refused.Load() < 3 {
		t.Fatalf("the bus refused %d events; the service did not try again", b.refused.Load())
	}
	if evs := f.settled(); len(evs) != 0 {
		t.Fatalf("events while the bus was down: %+v", evs)
	}
	if _, err := os.Stat(f.s.outboxPath()); err != nil {
		t.Fatalf("no outbox: %v", err)
	}
	b.down.Store(false)
	evs := f.events(1)
	if len(evs) != 1 || evs[0].Type != EvReady || decode[Marker](t, evs[0]).Digest != f.s.digest() {
		t.Fatalf("events after the bus came up = %+v", evs)
	}
	time.Sleep(retryFirst + 200*time.Millisecond)
	if evs := f.settled(); len(evs) != 1 {
		t.Fatalf("the state went out %d times", len(evs))
	}
	if _, err := os.Stat(f.s.outboxPath()); err == nil {
		t.Fatal("the outbox stayed after the bus took its events")
	}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %s", d)
	}
}

// A refused fw.env.changed waits as it is, and goes before the state. A
// state that the bus refused goes out as the state of the moment of the
// retry, so an older state never follows a newer one.
func TestRefusedEventsKeepOrderAndNewestState(t *testing.T) {
	f, b := newDownFixture(t, "packages:\n  python: [pyyaml]\n")
	a := f.agent()
	ctx := f.runCtx()
	if _, err := fw.ExtendToolEnv(ctx, f.cfg, nil); err != nil {
		t.Fatal(err)
	}
	first := f.s.digest()
	tl, err := newTool(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, ctx, tl, `{"command":"add","manager":"python","package":"six"}`); res.IsError {
		t.Fatalf("add = %+v", res)
	}
	second := f.s.digest()
	if second == first {
		t.Fatal("the add kept the digest")
	}
	o := outbox{}
	f.s.readOwned(f.s.outboxPath(), &o)
	if !o.State || len(o.Changed) != 1 || o.Changed[0].Type != EvChanged {
		t.Fatalf("outbox = %+v", o)
	}
	c, err := Start(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b.down.Store(false)
	evs := f.events(2)
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	// The in-memory bus delivers each event on a goroutine of its own, so
	// the order is the order in which the bus took them.
	if got := b.taken(); !slices.Equal(got, []string{EvChanged, EvReady}) {
		t.Fatalf("the bus took %v", got)
	}
	for _, e := range evs {
		switch e.Type {
		case EvChanged:
			if ch := decode[ChangedData](t, e); ch.Package != "six" || ch.Digest != second {
				t.Fatalf("changed = %+v", ch)
			}
		case EvReady:
			if m := decode[Marker](t, e); m.Digest != second {
				t.Fatalf("the state that went out is %s, not the newest %s", short(m.Digest), short(second))
			}
		}
	}
	time.Sleep(retryFirst + 200*time.Millisecond)
	if evs := f.settled(); len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
}

// A state event that the bus took clears the flag of a refusal before
// it, and keeps the flag of a refusal after it.
func TestSettledKeepsALaterRefusal(t *testing.T) {
	f := newFixture(t, fullBlock)
	in := newInstaller(nil, f.cfg, f.s)
	refusal := time.Now().UTC()
	if err := f.s.withOutbox(func(o *outbox) { o.State, o.StateAt = true, refusal }); err != nil {
		t.Fatal(err)
	}
	in.settled(refusal.Add(-time.Second))
	var o outbox
	if !f.s.readOwned(f.s.outboxPath(), &o) || !o.State {
		t.Fatal("a send that started before the refusal cleared it")
	}
	in.settled(refusal.Add(time.Second))
	if _, err := os.Stat(f.s.outboxPath()); err == nil {
		t.Fatal("a send that started after the refusal kept it")
	}
}

package fw

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// collect gathers the events that a watch delivers.
type collect struct {
	mu     sync.Mutex
	events []Event
}

func (c *collect) add(e Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
	return nil
}

func (c *collect) types(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		got := len(c.events)
		var out []string
		if got >= n {
			for _, e := range c.events {
				out = append(out, e.Type)
			}
		}
		c.mu.Unlock()
		if got >= n {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d events, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemBusRetainer(t *testing.T) {
	ctx := context.Background()
	b := NewMemBus()
	var _ Retainer = b
	ev := func(typ string) Event { return MustEvent("", typ, nil) }
	if err := b.Retain(ctx, "f.members.a", ev("old-a")); err != nil {
		t.Fatal(err)
	}
	_ = b.Retain(ctx, "f.members.a", ev("a"))
	_ = b.Retain(ctx, "f.members.b", ev("b"))
	_ = b.Retain(ctx, "f.alive.a", ev("alive"))

	var c collect
	stop, err := b.Watch(ctx, "f.members.*", c.add)
	if err != nil {
		t.Fatal(err)
	}
	// The kept events come first, sorted by subject, and only the newest
	// event of each subject is kept.
	if got := strings.Join(c.types(t, 2), ","); got != "a,b" {
		t.Fatalf("kept = %s", got)
	}
	_ = b.Retain(ctx, "f.members.c", ev("c"))
	_ = b.Retain(ctx, "f.alive.c", ev("alive-c"))
	if got := strings.Join(c.types(t, 3), ","); got != "a,b,c" {
		t.Fatalf("after retain = %s", got)
	}
	// A retained event does not reach a plain subscriber.
	var plain collect
	unsub, _ := b.Subscribe(ctx, "f.members.*", plain.add)
	defer unsub()
	_ = b.Retain(ctx, "f.members.d", ev("d"))
	c.types(t, 4)
	plain.mu.Lock()
	n := len(plain.events)
	plain.mu.Unlock()
	if n != 0 {
		t.Fatalf("a subscriber got %d retained events", n)
	}
	stop()
	stop()
	_ = b.Retain(ctx, "f.members.e", ev("e"))
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) != 4 {
		t.Fatalf("a stopped watch got %d events", len(c.events))
	}
}

package redis

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// retainBus opens a test bus with a hash of its own and deletes the hash at
// the end.
func retainBus(t *testing.T, url string) (*Bus, string) {
	t.Helper()
	b, root := testBus(t, url, map[string]string{"retainKey": "t:" + strings.ToLower(fw.NewID())})
	t.Cleanup(func() { _ = b.rdb.Del(context.Background(), b.retainKey).Err() })
	return b, root
}

func retain(t *testing.T, b *Bus, subject string, e fw.Event) {
	t.Helper()
	if err := b.Retain(context.Background(), subject, e); err != nil {
		t.Fatal(err)
	}
}

func startWatch(t *testing.T, b *Bus, pattern string) (<-chan fw.Event, func()) {
	t.Helper()
	ch := make(chan fw.Event, 16)
	stop, err := b.Watch(context.Background(), pattern, collect(ch))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return ch, stop
}

func TestRetainThenWatch(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	second, first := event(t, fw.EvFleetRecord), event(t, fw.EvFleetRecord)
	retain(t, b, root+".members.b", second)
	retain(t, b, root+".members.a", first)
	retain(t, b, root+".alive.a", event(t, fw.EvFleetAlive))
	retain(t, b, root+".members.a.extra", event(t, fw.EvFleetRecord))

	ch, _ := startWatch(t, b, root+".members.*")
	if got := recv(t, ch); got.ID != first.ID {
		t.Fatalf("got %s first, want the event of members.a %s", got.ID, first.ID)
	}
	if got := recv(t, ch); got.ID != second.ID {
		t.Fatalf("got %s second, want the event of members.b %s", got.ID, second.ID)
	}
	none(t, ch, 200*time.Millisecond)
}

func TestWatchThenRetain(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ch, _ := startWatch(t, b, root+".members.*")
	none(t, ch, 100*time.Millisecond)

	first, second := event(t, fw.EvFleetRecord), event(t, fw.EvFleetRecord)
	retain(t, b, root+".members.a", first)
	retain(t, b, root+".other.a", event(t, fw.EvFleetRecord))
	retain(t, b, root+".members.b", second)
	if got := recv(t, ch); got.ID != first.ID {
		t.Fatalf("got %s first, want %s", got.ID, first.ID)
	}
	if got := recv(t, ch); got.ID != second.ID {
		t.Fatalf("got %s second, want %s", got.ID, second.ID)
	}
	none(t, ch, 200*time.Millisecond)
}

func TestRetainReplaces(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	subject := root + ".members.a"
	retain(t, b, subject, event(t, fw.EvFleetRecord))
	newest := event(t, fw.EvFleetRecord)
	retain(t, b, subject, newest)

	ch, _ := startWatch(t, b, root+".>")
	if got := recv(t, ch); got.ID != newest.ID {
		t.Fatalf("got %s, want the newest event %s", got.ID, newest.ID)
	}
	none(t, ch, 200*time.Millisecond)
}

func TestWatchStop(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ch, stop := startWatch(t, b, root+".members.*")
	retain(t, b, root+".members.a", event(t, fw.EvFleetRecord))
	recv(t, ch)
	stop()
	stop() // a second call does nothing
	retain(t, b, root+".members.a", event(t, fw.EvFleetRecord))
	none(t, ch, 300*time.Millisecond)
}

// A retained event enters neither the stream of its subject nor a plain
// subscription.
func TestRetainSkipsStream(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ctx := context.Background()
	subject := root + ".members.a"
	sub := make(chan fw.Event, 4)
	stop, err := b.Subscribe(ctx, subject, collect(sub))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	retain(t, b, subject, event(t, fw.EvFleetRecord))
	none(t, sub, 200*time.Millisecond)
	n, err := b.rdb.Exists(ctx, subject).Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("Retain created the stream key %s", subject)
	}
}

func TestRetainRefusesWildcard(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	for _, subject := range []string{root + ".*", root + ".>", ""} {
		if err := b.Retain(context.Background(), subject, event(t, fw.EvFleetRecord)); err == nil {
			t.Errorf("Retain(%q) succeeded, want an error", subject)
		}
	}
}

func TestWatchDropsUndecodable(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ctx := context.Background()
	if err := b.rdb.HSet(ctx, b.retainKey, root+".members.a", "not json").Err(); err != nil {
		t.Fatal(err)
	}
	good := event(t, fw.EvFleetRecord)
	retain(t, b, root+".members.b", good)
	ch, _ := startWatch(t, b, root+".members.*")
	if got := recv(t, ch); got.ID != good.ID {
		t.Fatalf("got %s, want %s", got.ID, good.ID)
	}
	none(t, ch, 100*time.Millisecond)
}

func TestCloseStopsWatch(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ch, _ := startWatch(t, b, root+".members.*")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	none(t, ch, 100*time.Millisecond)
	if err := b.Retain(context.Background(), root+".members.a", event(t, fw.EvFleetRecord)); err == nil {
		t.Fatal("Retain after Close succeeded")
	}
	if _, err := b.Watch(context.Background(), root+".>", func(fw.Event) error { return nil }); err == nil {
		t.Fatal("Watch after Close succeeded")
	}
}

// A watch outlives the context of the call that started it.
func TestWatchOutlivesContext(t *testing.T) {
	url, _ := serverURL(t)
	b, root := retainBus(t, url)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan fw.Event, 4)
	stop, err := b.Watch(ctx, root+".members.*", collect(ch))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	cancel()
	e := event(t, fw.EvFleetRecord)
	retain(t, b, root+".members.a", e)
	if got := recv(t, ch); got.ID != e.ID {
		t.Fatalf("got %s, want %s", got.ID, e.ID)
	}
}

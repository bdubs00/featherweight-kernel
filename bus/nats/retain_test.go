package nats

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// retainBus opens a test bus with a bucket of its own and deletes the
// bucket at the end.
func retainBus(t *testing.T, url string) (*Bus, string) {
	t.Helper()
	b, root := testBus(t, url, map[string]string{"retainBucket": "R" + strings.ToUpper(fw.NewID())})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.js.DeleteKeyValue(ctx, b.retainBucket)
	})
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
	b, root := retainBus(t, serverURL(t))
	kept := event(t, "fw.fleet.record")
	retain(t, b, root+".members.a", kept)
	retain(t, b, root+".alive.a", event(t, "fw.fleet.alive"))

	ch, _ := startWatch(t, b, root+".members.*")
	if got := recv(t, ch); got.ID != kept.ID {
		t.Fatalf("got %s, want the kept event %s", got.ID, kept.ID)
	}
	none(t, ch, 200*time.Millisecond)
}

func TestWatchThenRetain(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	ch, _ := startWatch(t, b, root+".members.*")
	none(t, ch, 100*time.Millisecond)

	first, second := event(t, "fw.fleet.record"), event(t, "fw.fleet.record")
	retain(t, b, root+".members.a", first)
	retain(t, b, root+".other.a", event(t, "fw.fleet.record"))
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
	b, root := retainBus(t, serverURL(t))
	subject := root + ".members.a"
	retain(t, b, subject, event(t, "fw.fleet.record"))
	newest := event(t, "fw.fleet.record")
	retain(t, b, subject, newest)

	ch, _ := startWatch(t, b, root+".>")
	if got := recv(t, ch); got.ID != newest.ID {
		t.Fatalf("got %s, want the newest event %s", got.ID, newest.ID)
	}
	none(t, ch, 200*time.Millisecond)
}

func TestWatchStop(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	ch, stop := startWatch(t, b, root+".members.*")
	retain(t, b, root+".members.a", event(t, "fw.fleet.record"))
	recv(t, ch)
	stop()
	stop() // a second call does nothing
	retain(t, b, root+".members.a", event(t, "fw.fleet.record"))
	none(t, ch, 300*time.Millisecond)
}

// A retained event enters neither the stream nor a plain subscription,
// even on a subject that the stream captures.
func TestRetainSkipsStream(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	ctx := context.Background()
	stream, err := b.js.Stream(ctx, b.stream)
	if err != nil {
		t.Fatal(err)
	}
	before, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subject := root + ".fleet.members.a"
	if !b.captured(subject) {
		t.Fatalf("the stream does not capture %s", subject)
	}
	sub := make(chan fw.Event, 4)
	stop, err := b.Subscribe(ctx, subject, collect(sub))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	retain(t, b, subject, event(t, "fw.fleet.record"))
	none(t, sub, 200*time.Millisecond)
	after, err := stream.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.State.Msgs != before.State.Msgs {
		t.Fatalf("the stream holds %d messages after Retain, want %d", after.State.Msgs, before.State.Msgs)
	}
}

func TestRetainRefusesKey(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	for _, subject := range []string{root + ".a b", root + ".a:b", root + ".*", root + "..a", ""} {
		if err := b.Retain(context.Background(), subject, event(t, "fw.fleet.record")); err == nil {
			t.Errorf("Retain(%q) succeeded, want an error", subject)
		}
	}
}

func TestRetainBucketOption(t *testing.T) {
	url := serverURL(t)
	_, err := Open(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "nats", URL: url, Options: map[string]string{"retainBucket": "no.dots"}}})
	if err == nil || !strings.Contains(err.Error(), "retainBucket") {
		t.Fatalf("got %v, want an error that names retainBucket", err)
	}
}

// Two processes that create the bucket at the same time both succeed and
// share it.
func TestRetainSharedBucket(t *testing.T) {
	url := serverURL(t)
	bucket := "R" + strings.ToUpper(fw.NewID())
	open := func() *Bus {
		b, _ := testBus(t, url, map[string]string{"retainBucket": bucket})
		return b
	}
	a, c := open(), open()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.js.DeleteKeyValue(ctx, bucket)
	})
	root := "t" + strings.ToLower(fw.NewID())
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, b := range []*Bus{a, c} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = b.Retain(context.Background(), root+".members.p"+string(rune('0'+i)), event(t, "fw.fleet.record"))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("bus %d: %v", i, err)
		}
	}
	ch, _ := startWatch(t, a, root+".members.*")
	recv(t, ch)
	recv(t, ch)
}

func TestCloseStopsWatch(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	ch, _ := startWatch(t, b, root+".members.*")
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	left := len(b.watches)
	b.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d watches left after Close", left)
	}
	none(t, ch, 100*time.Millisecond)
	if err := b.Retain(context.Background(), root+".members.a", event(t, "fw.fleet.record")); err == nil {
		t.Fatal("Retain after Close succeeded")
	}
	if _, err := b.Watch(context.Background(), root+".>", func(fw.Event) error { return nil }); err == nil {
		t.Fatal("Watch after Close succeeded")
	}
}

// A watch outlives the context of the call that started it.
func TestWatchOutlivesContext(t *testing.T) {
	b, root := retainBus(t, serverURL(t))
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan fw.Event, 4)
	stop, err := b.Watch(ctx, root+".members.*", collect(ch))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	cancel()
	e := event(t, "fw.fleet.record")
	retain(t, b, root+".members.a", e)
	if got := recv(t, ch); got.ID != e.ID {
		t.Fatalf("got %s, want %s", got.ID, e.ID)
	}
}

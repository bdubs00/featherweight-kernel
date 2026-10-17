package redis

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestGlobPattern(t *testing.T) {
	cases := map[string]string{
		"fw.*.events":   "fw.*.events",
		"fw.>":          "fw.*",
		"fw.a.b":        "fw.a.b",
		"fw.*.x.>":      "fw.*.x.*",
		"fw.a?b.[c]":    `fw.a\?b.\[c\]`,
		`fw.back\slash`: `fw.back\\slash`,
		"fw.a*b":        `fw.a\*b`,
	}
	for in, want := range cases {
		if got := globPattern(in); got != want {
			t.Errorf("globPattern(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHasWildcard(t *testing.T) {
	for s, want := range map[string]bool{
		"fw.a":   false,
		"fw.*":   true,
		"fw.>":   true,
		"fw.a*":  false,
		"*":      true,
		"fw.*.b": true,
	} {
		if got := hasWildcard(s); got != want {
			t.Errorf("hasWildcard(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 250*time.Millisecond || backoff(2) != 500*time.Millisecond || backoff(100) != maxBackoff {
		t.Fatalf("backoff = %v %v %v", backoff(1), backoff(2), backoff(100))
	}
}

func TestOptions(t *testing.T) {
	for k, v := range map[string]string{"concurrency": "0", "maxlen": "lots"} {
		cfg := &fw.Config{Bus: fw.BusConfig{Driver: "redis", Options: map[string]string{k: v}}}
		if _, err := Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("Open with %s=%s: %v", k, v, err)
		}
	}
}

func TestURLErrorHidesPassword(t *testing.T) {
	cfg := &fw.Config{Bus: fw.BusConfig{Driver: "redis", URL: "redis://user:hunter2@host:6379/0\x7f"}}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("Open with a bad URL succeeded")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error shows the password: %v", err)
	}
}

// Integration tests. They use REDIS_URL when it is set and miniredis
// otherwise.

func serverURL(t *testing.T) (string, *miniredis.Miniredis) {
	if u := os.Getenv("REDIS_URL"); u != "" {
		return u, nil
	}
	m := miniredis.RunT(t)
	return "redis://" + m.Addr() + "/0", m
}

// testBus opens a bus with short timings and its own subject space, so that
// tests do not meet on a shared server. It returns the bus and the subject
// root.
func testBus(t *testing.T, url string, opts map[string]string) (*Bus, string) {
	t.Helper()
	root := "t" + strings.ToLower(fw.NewID())
	b, err := Open(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "redis", URL: url, Options: opts}})
	if err != nil {
		t.Fatal(err)
	}
	b.block = 100 * time.Millisecond
	b.claimEvery = 50 * time.Millisecond
	b.claimIdle = 300 * time.Millisecond
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, err := b.rdb.Keys(ctx, root+".*").Result(); err == nil && len(keys) > 0 {
			_ = b.rdb.Del(ctx, keys...).Err()
		}
		_ = b.Close()
	})
	return b, root
}

func event(t *testing.T, typ string) fw.Event {
	t.Helper()
	return fw.MustEvent("run-"+fw.NewID(), typ, map[string]string{"k": "v"})
}

func publish(t *testing.T, b fw.Bus, subject string, e fw.Event) {
	t.Helper()
	if err := b.Publish(context.Background(), subject, e); err != nil {
		t.Fatal(err)
	}
}

func recv(t *testing.T, ch <-chan fw.Event) fw.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		return fw.Event{}
	}
}

func none(t *testing.T, ch <-chan fw.Event, wait time.Duration) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event %s %s", e.ID, e.Type)
	case <-time.After(wait):
	}
}

func collect(ch chan fw.Event) func(fw.Event) error {
	return func(e fw.Event) error {
		ch <- e
		return nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pendingCount(t *testing.T, b *Bus, queue string) int64 {
	t.Helper()
	p, err := b.rdb.XPending(context.Background(), queue, b.group).Result()
	if err != nil {
		t.Fatal(err)
	}
	return p.Count
}

func TestRegistered(t *testing.T) {
	url, _ := serverURL(t)
	bus, err := fw.OpenBus(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "redis", URL: url}})
	if err != nil {
		t.Fatal(err)
	}
	_ = bus.(*Bus).Close()
}

func TestPublishFields(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, map[string]string{"maxlen": "10"})
	e := event(t, fw.EvRunFinished)
	publish(t, b, root+".f", e)
	msgs, err := b.rdb.XRange(context.Background(), root+".f", "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Values["type"] != e.Type || msgs[0].Values["run"] != e.Subject {
		t.Fatalf("stream entries = %+v", msgs)
	}
	if err := b.Publish(context.Background(), root+".*", e); err == nil {
		t.Fatal("Publish to a wildcard succeeded")
	}
}

func TestPublishSubscribeFanOut(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	ctx := context.Background()
	a, c, all, exact := make(chan fw.Event, 8), make(chan fw.Event, 8), make(chan fw.Event, 8), make(chan fw.Event, 8)
	for _, s := range []struct {
		subject string
		ch      chan fw.Event
	}{{root + ".*.events", a}, {root + ".*.events", c}, {root + ".>", all}, {root + ".x.events", exact}} {
		stop, err := b.Subscribe(ctx, s.subject, collect(s.ch))
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
	}
	e := event(t, fw.EvTurnStarted)
	publish(t, b, root+".x.events", e)
	for _, ch := range []chan fw.Event{a, c, all, exact} {
		got := recv(t, ch)
		if got.ID != e.ID || got.Type != e.Type || string(got.Data) != string(e.Data) {
			t.Fatalf("got %+v, want %+v", got, e)
		}
	}
	// A Redis "*" matches dots, but "*" in a subject is one token only.
	deep := event(t, fw.EvTurnFinished)
	publish(t, b, root+".x.y.events", deep)
	if got := recv(t, all); got.ID != deep.ID {
		t.Fatalf("got %s, want %s", got.ID, deep.ID)
	}
	none(t, a, 200*time.Millisecond)
	none(t, c, 0)
	none(t, exact, 0)
}

func TestSubscribeDropsUndecodable(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	got := make(chan fw.Event, 2)
	stop, err := b.Subscribe(context.Background(), root+".bad", collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := b.rdb.Publish(context.Background(), root+".bad", "not json").Err(); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvError)
	publish(t, b, root+".bad", e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

func TestConsumeAck(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	queue := root + ".jobs.a"
	// An event published before the first consumer is not lost.
	early := event(t, fw.EvRunQueued)
	publish(t, b, queue, early)
	got := make(chan fw.Event, 8)
	stop, err := b.Consume(context.Background(), queue, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	want := map[string]bool{early.ID: true}
	for range 3 {
		e := event(t, fw.EvRunQueued)
		want[e.ID] = true
		publish(t, b, queue, e)
	}
	for range 4 {
		e := recv(t, got)
		if !want[e.ID] {
			t.Fatalf("unexpected or repeated event %s", e.ID)
		}
		delete(want, e.ID)
	}
	eventually(t, "all acks", func() bool { return pendingCount(t, b, queue) == 0 })
	none(t, got, 500*time.Millisecond)
}

func TestConsumeRejectsPattern(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	if _, err := b.Consume(context.Background(), root+".jobs.*", collect(nil)); err == nil {
		t.Fatal("Consume on a pattern succeeded")
	}
}

func TestConsumeRetry(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	queue := root + ".jobs.r"
	var calls atomic.Int32
	done := make(chan fw.Event, 4)
	stop, err := b.Consume(context.Background(), queue, func(e fw.Event) error {
		if calls.Add(1) == 1 {
			return errors.New("first attempt fails")
		}
		done <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	if r := recv(t, done); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
	eventually(t, "the ack", func() bool { return pendingCount(t, b, queue) == 0 })
	if n := calls.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2", n)
	}
}

func TestConsumeGivesUp(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	queue := root + ".jobs.g"
	var calls atomic.Int32
	stop, err := b.Consume(context.Background(), queue, func(fw.Event) error {
		calls.Add(1)
		return errors.New("always fails")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	publish(t, b, queue, event(t, fw.EvRunQueued))
	eventually(t, "the last delivery to be acknowledged", func() bool {
		return calls.Load() >= maxDeliver && pendingCount(t, b, queue) == 0
	})
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != maxDeliver {
		t.Fatalf("handler ran %d times, want %d", n, maxDeliver)
	}
}

func TestConsumeClaimsFromDeadConsumer(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	ctx := context.Background()
	queue := root + ".jobs.dead"
	if err := b.ensureGroup(ctx, queue); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	// Another process reads the message and dies without an ack.
	if err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: b.group, Consumer: "dead-1", Streams: []string{queue, ">"}, Count: 1,
	}).Err(); err != nil {
		t.Fatal(err)
	}
	got := make(chan fw.Event, 2)
	stop, err := b.Consume(ctx, queue, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
	eventually(t, "the ack", func() bool { return pendingCount(t, b, queue) == 0 })
}

func TestLongHandlerKeepsMessage(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	queue := root + ".jobs.long"
	var calls atomic.Int32
	done := make(chan fw.Event, 4)
	stop, err := b.Consume(context.Background(), queue, func(e fw.Event) error {
		calls.Add(1)
		time.Sleep(4 * b.claimIdle) // far past the idle time
		done <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	publish(t, b, queue, event(t, fw.EvRunQueued))
	recv(t, done)
	eventually(t, "the ack", func() bool { return pendingCount(t, b, queue) == 0 })
	if n := calls.Load(); n != 1 {
		t.Fatalf("handler ran %d times, want 1", n)
	}
}

func TestConsumeConcurrency(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, map[string]string{"concurrency": "2"})
	queue := root + ".jobs.c"
	var running, peak atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	stop, err := b.Consume(context.Background(), queue, func(fw.Event) error {
		defer wg.Done()
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	for range 3 {
		publish(t, b, queue, event(t, fw.EvRunQueued))
	}
	eventually(t, "two handlers running", func() bool { return running.Load() == 2 })
	time.Sleep(300 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency %d, want 2", p)
	}
	close(release)
	wg.Wait()
	eventually(t, "all acks", func() bool { return pendingCount(t, b, queue) == 0 })
}

func TestStop(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	ctx := context.Background()

	sub := make(chan fw.Event, 4)
	stopSub, err := b.Subscribe(ctx, root+".s", collect(sub))
	if err != nil {
		t.Fatal(err)
	}
	stopSub()
	stopSub() // a second call is safe
	publish(t, b, root+".s", event(t, fw.EvTurnStarted))
	none(t, sub, 300*time.Millisecond)

	queue := root + ".jobs.s"
	first := make(chan fw.Event, 4)
	stop, err := b.Consume(ctx, queue, collect(first))
	if err != nil {
		t.Fatal(err)
	}
	stop()
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	none(t, first, 300*time.Millisecond)

	// The queue is durable: the next consumer receives the event.
	second := make(chan fw.Event, 4)
	stop2, err := b.Consume(ctx, queue, collect(second))
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	if r := recv(t, second); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

func TestStopWaitsForHandlers(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	queue := root + ".jobs.w"
	started := make(chan struct{})
	var finished atomic.Bool
	stop, err := b.Consume(context.Background(), queue, func(fw.Event) error {
		close(started)
		time.Sleep(200 * time.Millisecond)
		finished.Store(true)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	publish(t, b, queue, event(t, fw.EvRunQueued))
	<-started
	stop()
	if !finished.Load() {
		t.Fatal("stop returned before the handler finished")
	}
	if n := pendingCount(t, b, queue); n != 0 {
		t.Fatalf("%d messages pending after stop", n)
	}
}

func TestClose(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	ctx := context.Background()
	if _, err := b.Consume(ctx, root+".jobs.x", collect(make(chan fw.Event, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Subscribe(ctx, root+".s", collect(make(chan fw.Event, 1))); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(ctx, root+".x", event(t, fw.EvError)); err == nil {
		t.Fatal("Publish after Close succeeded")
	}
	if _, err := b.Consume(ctx, root+".jobs.y", collect(nil)); !errors.Is(err, errClosed) {
		t.Fatalf("Consume after Close: %v", err)
	}
}

func TestServerRestart(t *testing.T) {
	url, m := serverURL(t)
	if m == nil {
		t.Skip("the restart test needs miniredis")
	}
	b, root := testBus(t, url, nil)
	ctx := context.Background()
	sub := make(chan fw.Event, 8)
	stopSub, err := b.Subscribe(ctx, root+".ev", collect(sub))
	if err != nil {
		t.Fatal(err)
	}
	defer stopSub()
	jobs := make(chan fw.Event, 8)
	stop, err := b.Consume(ctx, root+".jobs", collect(jobs))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Start a new, empty server on the same address: a restart without
	// persistence. (miniredis Restart keeps the data, but its blocking
	// commands stop answering after a restart.)
	addr := m.Addr()
	m.Close()
	time.Sleep(300 * time.Millisecond)
	m2 := miniredis.NewMiniRedis()
	if err := m2.StartAddr(addr); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m2.Close)

	j := event(t, fw.EvRunQueued)
	eventually(t, "a publish after the restart", func() bool {
		return b.Publish(ctx, root+".jobs", j) == nil
	})
	if r := recv(t, jobs); r.ID != j.ID {
		t.Fatalf("got %s, want %s", r.ID, j.ID)
	}
	// The subscription comes back on its own. Publish until it sees one.
	e := event(t, fw.EvTurnStarted)
	eventually(t, "the subscription after the restart", func() bool {
		publish(t, b, root+".ev", e)
		select {
		case r := <-sub:
			return r.ID == e.ID
		case <-time.After(200 * time.Millisecond):
			return false
		}
	})
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	url, _ := serverURL(t)
	b, root := testBus(t, url, nil)
	release := make(chan struct{})
	defer close(release)
	stop, err := b.Subscribe(context.Background(), root+".slow", func(fw.Event) error {
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	start := time.Now()
	for range 200 {
		publish(t, b, root+".slow", event(t, fw.EvToolProgress))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("200 publishes took %v with a blocked subscriber", d)
	}
}

// TestOpenBeforeTheServer opens the bus while no server listens, as under
// ax, where the sandbox has no egress until fw reports ready. Open returns
// a bus, and it works once the server starts.
func TestOpenBeforeTheServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	b, err := Open(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "redis", URL: "redis://" + addr + "/0"}})
	if err != nil {
		t.Fatalf("Open with no server = %v", err)
	}
	defer b.Close()
	m := miniredis.NewMiniRedis()
	if err := m.StartAddr(addr); err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := b.Publish(context.Background(), "late.subject", fw.MustEvent("r1", "test.event", nil)); err != nil {
		t.Fatalf("Publish after the server started = %v", err)
	}
}

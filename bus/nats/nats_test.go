package nats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestDurableName(t *testing.T) {
	cases := map[string]string{
		"fw.jobs.reviewer": "fw-fw-jobs-reviewer",
		"jobs":             "fw-jobs",
		"fw.jobs.*":        "fw-fw-jobs-_",
		"fw.>":             "fw-fw-__",
		"a b/c":            "fw-a_b_c",
	}
	for in, want := range cases {
		if got := durableName(in); got != want {
			t.Errorf("durableName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCovers(t *testing.T) {
	cases := []struct {
		filter, pattern string
		want            bool
	}{
		{"fw.>", "fw.jobs.a", true},
		{"fw.>", "fw.jobs", true},
		{"fw.>", "fw", false},
		{"fw.>", "fw.*", true},
		{"fw.>", "fw.>", true},
		{"fw.*", "fw.a", true},
		{"fw.*", "fw.a.b", false},
		{"fw.*", "fw.*", true},
		{"fw.*", "fw.>", false},
		{"fw.a", "fw.*", false},
		{"fw.*.x", "fw.*.x", true},
		{"fw.a.>", "fw.*.b", false},
		{">", "anything.at.all", true},
		{"jobs", "jobs", true},
		{"jobs", "jobs.a", false},
	}
	for _, c := range cases {
		if got := covers(c.filter, c.pattern); got != c.want {
			t.Errorf("covers(%q, %q) = %v, want %v", c.filter, c.pattern, got, c.want)
		}
	}
}

func TestStreamSubjects(t *testing.T) {
	cases := []struct {
		list, intake, egress, dead string
		want                       []string
	}{
		{"", "", "", "", []string{"fw.>"}},
		{"", "fw.jobs.reviewer", "fw.events", "fw.jobs.reviewer.dead", []string{"fw.>"}},
		{"", "jobs.reviewer", "", "jobs.reviewer.dead", []string{"fw.>", "jobs.>"}},
		{"", "", "events", "", []string{"fw.>", "events.>"}},
		{"", "", "acme.events", "", []string{"fw.>", "acme.>"}},
		{"", "jobs", "", "", []string{"fw.>", "jobs"}},
		// A one-token intake does not capture its dead letter.
		{"", "jobs", "", "jobs.dead", []string{"fw.>", "jobs", "jobs.>"}},
		{" a.> , b.* ", "b.x", "a", "b.x.dead", []string{"a.>", "b.*", "b.x.dead"}},
		{"a.>", "jobs.x", "jobs.out", "jobs.x.dead", []string{"a.>", "jobs.>"}},
		// A list that names the intake alone gets the dead letter itself,
		// because a stream refuses filters that overlap, such as fw.>.
		{"fw.jobs.migrator", "fw.jobs.migrator", "", "fw.jobs.migrator.dead", []string{"fw.jobs.migrator", "fw.jobs.migrator.dead"}},
	}
	for _, c := range cases {
		got := streamSubjects(c.list, c.intake, c.egress, c.dead)
		if !slices.Equal(got, c.want) {
			t.Errorf("streamSubjects(%q, %q, %q, %q) = %v, want %v", c.list, c.intake, c.egress, c.dead, got, c.want)
		}
	}
}

func TestOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"fw.>", "fw.jobs.a", true},
		{"fw.jobs.a", "fw.>", true},
		{"fw.>", "fw", false},
		{"fw.jobs.a", "fw.jobs.a.dead", false},
		{"fw.*.a", "fw.jobs.*", true},
		{"fw.*", "fw.a.b", false},
		{"a.>", "b.>", false},
		{">", "x", true},
	}
	for _, c := range cases {
		if got := overlaps(c.a, c.b); got != c.want {
			t.Errorf("overlaps(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestMergeSubjects(t *testing.T) {
	got := mergeSubjects([]string{"fw.>"}, []string{"fw.a", "jobs.>", "jobs.x"})
	if want := []string{"fw.>", "jobs.>"}; !slices.Equal(got, want) {
		t.Fatalf("mergeSubjects = %v, want %v", got, want)
	}
}

func TestCheckSubject(t *testing.T) {
	for _, s := range []string{"", "fw.*", "fw.>", "fw..a", ".fw"} {
		if checkSubject(s) == nil {
			t.Errorf("checkSubject(%q) = nil, want an error", s)
		}
	}
	if err := checkSubject("fw.events.a"); err != nil {
		t.Errorf("checkSubject: %v", err)
	}
}

func TestMsgID(t *testing.T) {
	if msgID("fw.a", "01X") == msgID("fw.b", "01X") {
		t.Fatal("the same event on two subjects has one dedup id")
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 250*time.Millisecond || backoff(2) != 500*time.Millisecond || backoff(100) != maxBackoff {
		t.Fatalf("backoff = %v %v %v", backoff(1), backoff(2), backoff(100))
	}
}

func TestOptions(t *testing.T) {
	cfg := &fw.Config{Bus: fw.BusConfig{Driver: "nats", Options: map[string]string{"concurrency": "zero"}}}
	if _, err := Open(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "concurrency") {
		t.Fatalf("Open with a bad concurrency: %v", err)
	}
	cfg = &fw.Config{Bus: fw.BusConfig{Driver: "nats", Options: map[string]string{"token": "FW_TEST_NO_SUCH_TOKEN"}}}
	if _, err := Open(context.Background(), cfg); !errors.Is(err, fw.ErrSecretNotFound) {
		t.Fatalf("Open with a missing token secret: %v", err)
	}
}

// Integration tests. They use NATS_URL when it is set and an embedded
// JetStream server otherwise.

func runServer(t *testing.T, dir string, port int) *server.Server {
	t.Helper()
	s, err := server.NewServer(&server.Options{
		Host:      "127.0.0.1",
		Port:      port,
		JetStream: true,
		StoreDir:  dir,
		NoLog:     true,
		NoSigs:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not start")
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	return s
}

func serverURL(t *testing.T) string {
	if u := os.Getenv("NATS_URL"); u != "" {
		return u
	}
	return runServer(t, t.TempDir(), -1).ClientURL()
}

// testBus opens a bus with its own stream and subject space, so that tests
// do not meet on a shared server. It returns the bus and the subject root.
func testBus(t *testing.T, url string, opts map[string]string) (*Bus, string) {
	t.Helper()
	root := "t" + strings.ToLower(fw.NewID())
	o := map[string]string{"stream": strings.ToUpper(root), "subjects": root + ".>"}
	for k, v := range opts {
		o[k] = v
	}
	b, err := Open(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "nats", URL: url, Options: o}})
	if err != nil {
		t.Fatal(err)
	}
	b.fetchWait = 200 * time.Millisecond
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.js.DeleteStream(ctx, b.stream)
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

func consumerInfo(t *testing.T, b *Bus, queue string) *jetstream.ConsumerInfo {
	t.Helper()
	ctx := context.Background()
	c, err := b.js.Consumer(ctx, b.stream, durableName(queue))
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func settled(t *testing.T, b *Bus, queue string) func() bool {
	return func() bool {
		info := consumerInfo(t, b, queue)
		return info.NumAckPending == 0 && info.NumPending == 0
	}
}

func TestRegistered(t *testing.T) {
	url := serverURL(t)
	root := "t" + strings.ToLower(fw.NewID())
	bus, err := fw.OpenBus(context.Background(), &fw.Config{Bus: fw.BusConfig{
		Driver: "nats", URL: url,
		Options: map[string]string{"stream": strings.ToUpper(root), "subjects": root + ".>"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b := bus.(*Bus)
	_ = b.js.DeleteStream(context.Background(), b.stream)
	_ = b.Close()
}

func TestStreamTakesIntakeAndEgress(t *testing.T) {
	url := serverURL(t)
	root := "t" + strings.ToLower(fw.NewID())
	cfg := &fw.Config{Bus: fw.BusConfig{
		Driver: "nats", URL: url,
		Intake:  "j" + root + ".reviewer",
		Egress:  "e" + root,
		Options: map[string]string{"stream": strings.ToUpper(root), "subjects": root + ".>"},
	}}
	b1, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	b1.fetchWait = 200 * time.Millisecond
	defer func() {
		_ = b1.js.DeleteStream(context.Background(), b1.stream)
		_ = b1.Close()
	}()
	want := []string{root + ".>", "j" + root + ".>", "e" + root + ".>"}
	s, err := b1.js.Stream(context.Background(), b1.stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CachedInfo().Config.Subjects; !slices.Equal(got, want) {
		t.Fatalf("stream subjects = %v, want %v", got, want)
	}

	// A second process with another intake adds to the subjects and keeps
	// the first ones.
	cfg.Bus.Intake = "k" + root + ".writer"
	b2, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	s, err = b2.js.Stream(context.Background(), b2.stream)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, "k"+root+".>")
	if got := s.CachedInfo().Config.Subjects; !slices.Equal(got, want) {
		t.Fatalf("stream subjects = %v, want %v", got, want)
	}

	// The intake queue works end to end.
	got := make(chan fw.Event, 1)
	stop, err := b1.Consume(context.Background(), "j"+root+".reviewer", collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := event(t, fw.EvRunQueued)
	publish(t, b2, "j"+root+".reviewer", e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

func TestPublishSubscribeFanOut(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	ctx := context.Background()
	a, c, all := make(chan fw.Event, 8), make(chan fw.Event, 8), make(chan fw.Event, 8)
	for _, s := range []struct {
		subject string
		ch      chan fw.Event
	}{{root + ".*.events", a}, {root + ".*.events", c}, {root + ".>", all}} {
		stop, err := b.Subscribe(ctx, s.subject, collect(s.ch))
		if err != nil {
			t.Fatal(err)
		}
		defer stop()
	}
	e := event(t, fw.EvTurnStarted)
	publish(t, b, root+".x.events", e)
	for _, ch := range []chan fw.Event{a, c, all} {
		got := recv(t, ch)
		if got.ID != e.ID || got.Type != e.Type || string(got.Data) != string(e.Data) {
			t.Fatalf("got %+v, want %+v", got, e)
		}
	}
	other := event(t, fw.EvTurnFinished)
	publish(t, b, root+".x.other", other)
	if got := recv(t, all); got.ID != other.ID {
		t.Fatalf("got %s, want %s", got.ID, other.ID)
	}
	none(t, a, 200*time.Millisecond)
	none(t, c, 0)
}

func TestHeaders(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	e := event(t, fw.EvRunFinished)
	publish(t, b, root+".h", e)
	s, err := b.js.Stream(context.Background(), b.stream)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.GetLastMsgForSubject(context.Background(), root+".h")
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("Fw-Type") != e.Type || m.Header.Get("Fw-Run") != e.Subject || m.Header.Get(jetstream.MsgIDHeader) != msgID(root+".h", e.ID) {
		t.Fatalf("headers = %v", m.Header)
	}
}

func TestPublishOutsideStream(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	subject := "o" + root + ".x"
	got := make(chan fw.Event, 1)
	stop, err := b.Subscribe(context.Background(), subject, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := event(t, fw.EvToolProgress)
	publish(t, b, subject, e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

func TestSubscribeDropsUndecodable(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	got := make(chan fw.Event, 2)
	stop, err := b.Subscribe(context.Background(), root+".bad", collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := b.nc.Publish(root+".bad", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	e := event(t, fw.EvError)
	publish(t, b, root+".bad", e)
	if r := recv(t, got); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
}

func TestConsumeAck(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	queue := root + ".jobs.a"
	got := make(chan fw.Event, 8)
	stop, err := b.Consume(context.Background(), queue, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	want := map[string]bool{}
	for range 3 {
		e := event(t, fw.EvRunQueued)
		want[e.ID] = true
		publish(t, b, queue, e)
	}
	for range 3 {
		e := recv(t, got)
		if !want[e.ID] {
			t.Fatalf("unexpected or repeated event %s", e.ID)
		}
		delete(want, e.ID)
	}
	eventually(t, "all acks", settled(t, b, queue))
	none(t, got, 300*time.Millisecond)
}

func TestConsumeDedup(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	queue := root + ".jobs.d"
	got := make(chan fw.Event, 8)
	stop, err := b.Consume(context.Background(), queue, collect(got))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	e := event(t, fw.EvRunQueued)
	publish(t, b, queue, e)
	publish(t, b, queue, e)
	recv(t, got)
	none(t, got, 500*time.Millisecond)
}

func TestConsumeRetry(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
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
	eventually(t, "the ack", settled(t, b, queue))
	if n := calls.Load(); n != 2 {
		t.Fatalf("handler ran %d times, want 2", n)
	}
}

func TestConsumeGivesUp(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
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
	eventually(t, "five deliveries", func() bool { return calls.Load() >= maxDeliver })
	time.Sleep(500 * time.Millisecond)
	if n := calls.Load(); n != maxDeliver {
		t.Fatalf("handler ran %d times, want %d", n, maxDeliver)
	}
}

func TestConsumeConcurrency(t *testing.T) {
	b, root := testBus(t, serverURL(t), map[string]string{"concurrency": "2"})
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
	eventually(t, "all acks", settled(t, b, queue))
}

func TestStop(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
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
	b, root := testBus(t, serverURL(t), nil)
	queue := root + ".jobs.w"
	started := make(chan struct{})
	var finished atomic.Bool
	stop, err := b.Consume(context.Background(), queue, func(fw.Event) error {
		close(started)
		time.Sleep(300 * time.Millisecond)
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
	eventually(t, "the ack", settled(t, b, queue))
}

func TestClose(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
	if _, err := b.Consume(context.Background(), root+".jobs.x", collect(make(chan fw.Event, 1))); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(context.Background(), root+".x", event(t, fw.EvError)); err == nil {
		t.Fatal("Publish after Close succeeded")
	}
	if _, err := b.Consume(context.Background(), root+".jobs.y", collect(nil)); !errors.Is(err, errClosed) {
		t.Fatalf("Consume after Close: %v", err)
	}
}

func TestServerRestart(t *testing.T) {
	if os.Getenv("NATS_URL") != "" {
		t.Skip("the restart test needs the embedded server")
	}
	dir := t.TempDir()
	s := runServer(t, dir, -1)
	port := s.Addr().(*net.TCPAddr).Port
	b, root := testBus(t, s.ClientURL(), nil)
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

	reconnected := make(chan struct{}, 1)
	b.nc.SetReconnectHandler(func(*nats.Conn) {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})
	s.Shutdown()
	s.WaitForShutdown()
	runServer(t, dir, port)
	select {
	case <-reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("the client did not reconnect")
	}

	e := event(t, fw.EvTurnStarted)
	publish(t, b, root+".ev", e)
	if r := recv(t, sub); r.ID != e.ID {
		t.Fatalf("got %s, want %s", r.ID, e.ID)
	}
	j := event(t, fw.EvRunQueued)
	publish(t, b, root+".jobs", j)
	if r := recv(t, jobs); r.ID != j.ID {
		t.Fatalf("got %s, want %s", r.ID, j.ID)
	}
}

func TestServerRestartWithoutStorage(t *testing.T) {
	if os.Getenv("NATS_URL") != "" {
		t.Skip("the restart test needs the embedded server")
	}
	s := runServer(t, t.TempDir(), -1)
	port := s.Addr().(*net.TCPAddr).Port
	b, root := testBus(t, s.ClientURL(), nil)
	ctx := context.Background()
	jobs := make(chan fw.Event, 8)
	stop, err := b.Consume(ctx, root+".jobs", collect(jobs))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	reconnected := make(chan struct{}, 1)
	b.nc.SetReconnectHandler(func(*nats.Conn) {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})
	s.Shutdown()
	s.WaitForShutdown()
	runServer(t, t.TempDir(), port) // the stream and the consumer are gone
	select {
	case <-reconnected:
	case <-time.After(15 * time.Second):
		t.Fatal("the client did not reconnect")
	}

	// Publish makes the stream again, and the consume loop makes the
	// consumer again.
	j := event(t, fw.EvRunQueued)
	publish(t, b, root+".jobs", j)
	if r := recv(t, jobs); r.ID != j.ID {
		t.Fatalf("got %s, want %s", r.ID, j.ID)
	}
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b, root := testBus(t, serverURL(t), nil)
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
// a bus, and the stream exists once the server starts.
func TestOpenBeforeTheServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	root := "t" + strings.ToLower(fw.NewID())
	b, err := Open(context.Background(), &fw.Config{Bus: fw.BusConfig{Driver: "nats", URL: fmt.Sprintf("nats://127.0.0.1:%d", port),
		Options: map[string]string{"stream": strings.ToUpper(root), "subjects": root + ".>"}}})
	if err != nil {
		t.Fatalf("Open with no server = %v", err)
	}
	defer b.Close()
	runServer(t, t.TempDir(), port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := b.js.Stream(ctx, b.stream)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stream did not appear after the server started: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !b.captured(root + ".x") {
		t.Fatal("the bus does not know the subjects of its stream after the late connect")
	}
}

// Package nats is the NATS bus driver. It registers itself as "nats".
//
// Publish stores each event in one JetStream stream. Subscribe is a core NATS
// subscription: fan-out with no durability. Consume is a durable JetStream
// pull consumer with at-least-once delivery.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() {
	fw.RegisterBus("nats", func(ctx context.Context, cfg *fw.Config) (fw.Bus, error) {
		b, err := Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
}

const (
	defaultStream      = "FW"
	defaultSubjects    = "fw.>"
	defaultConcurrency = 4

	// ackWait is long because a handler can hold a message for a whole run.
	// A handler that runs longer still tells the server it is in progress.
	ackWait          = 5 * time.Minute
	maxDeliver       = 5
	defaultFetchWait = 2 * time.Second
	flushTimeout     = 5 * time.Second
	recoverTimeout   = 10 * time.Second
	maxBackoff       = 5 * time.Second

	headerType = "Fw-Type"
	headerRun  = "Fw-Run"
)

var errClosed = errors.New("nats: bus is closed")

// Bus is a fw.Bus on NATS with JetStream.
type Bus struct {
	nc          *nats.Conn
	js          jetstream.JetStream
	stream      string
	concurrency int
	log         atomic.Pointer[slog.Logger]

	// fetchWait bounds one pull request. Stop waits at most this long for a
	// pull in flight. Tests shorten it.
	fetchWait time.Duration
	ackWait   time.Duration

	ensureMu sync.Mutex // serializes stream changes

	mu        sync.Mutex
	subjects  []string // the subjects that the stream captures
	consumers map[*consumer]struct{}
	closed    bool
}

// errNotConnected means that the connection to the server is not up yet.
// Consume and a stream publish fail with it at once, so a caller tries
// again later instead of waiting for a request timeout.
var errNotConnected = errors.New("nats: not connected yet")

// Open connects to NATS and makes sure the stream exists.
//
// It reads cfg.Bus.URL (default nats://127.0.0.1:4222), cfg.Bus.Intake,
// cfg.Bus.Egress, and these keys of cfg.Bus.Options:
//
//   - stream: the JetStream stream name (default FW)
//   - subjects: comma-separated subject filters of the stream (default fw.>)
//   - credentials: the path to a .creds file
//   - token: the name of a secret that holds the auth token, read from the
//     environment by fw.EnvSecrets
//   - concurrency: the number of messages that one Consume handles at a time
//     (default 4)
func Open(ctx context.Context, cfg *fw.Config) (*Bus, error) {
	if cfg == nil {
		return nil, errors.New("nats: no config")
	}
	bc := cfg.Bus
	opt := bc.Options
	conc, err := intOption(opt, "concurrency", defaultConcurrency)
	if err != nil {
		return nil, err
	}
	stream := strings.TrimSpace(opt["stream"])
	if stream == "" {
		stream = defaultStream
	}
	b := &Bus{
		stream:      stream,
		concurrency: conc,
		fetchWait:   defaultFetchWait,
		ackWait:     ackWait,
		consumers:   map[*consumer]struct{}{},
	}
	b.SetLogger(slog.Default())

	subjects := streamSubjects(opt["subjects"], bc.Intake, bc.Egress)
	// ready closes when Open has set b.js, because the connect handler can
	// run before Open returns.
	ready := make(chan struct{})
	nopts := []nats.Option{
		nats.Name("fw"),
		nats.MaxReconnects(-1),
		// A server that is not reachable at start does not stop the
		// process. Under ax, the sandbox has no egress until the process
		// reports ready, so the first attempt always fails there. The
		// client keeps trying, and the stream is made at the first connect.
		nats.RetryOnFailedConnect(true),
		nats.ConnectHandler(func(*nats.Conn) {
			go func() {
				<-ready
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := b.ensureStream(ctx, subjects); err != nil {
					b.logger().Warn("bus stream not ready", "error", err)
					return
				}
				b.logger().Info("bus connected")
			}()
		}),
		nats.ReconnectWait(time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				b.logger().Warn("bus disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) {
			b.logger().Info("bus reconnected")
		}),
		nats.ErrorHandler(func(_ *nats.Conn, s *nats.Subscription, err error) {
			subject := ""
			if s != nil {
				subject = s.Subject
			}
			b.logger().Warn("bus error", "subject", subject, "error", err)
		}),
	}
	if p := strings.TrimSpace(opt["credentials"]); p != "" {
		nopts = append(nopts, nats.UserCredentials(p))
	}
	if name := strings.TrimSpace(opt["token"]); name != "" {
		tok, err := fw.EnvSecrets{}.Get(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("nats: token: %w", err)
		}
		nopts = append(nopts, nats.Token(tok))
	}
	addr := bc.URL
	if addr == "" {
		addr = nats.DefaultURL
	}
	nc, err := nats.Connect(addr, nopts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect: %w", redact(err))
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("nats: jetstream: %w", err)
	}
	b.nc, b.js = nc, js
	close(ready)
	if !nc.IsConnected() {
		b.logger().Warn("bus not reachable yet; retrying in the background", "error", redact(nc.LastError()))
		return b, nil
	}
	if err := b.ensureStream(ctx, subjects); err != nil {
		nc.Close()
		return nil, err
	}
	return b, nil
}

// SetLogger sets the logger for connection events and handler failures. The
// default is slog.Default(). Log lines carry ids, types, and subjects, never
// event data.
func (b *Bus) SetLogger(l *slog.Logger) {
	if l != nil {
		b.log.Store(l.With("bus", "nats"))
	}
}

func (b *Bus) logger() *slog.Logger { return b.log.Load() }

// Close stops every consumer loop and closes the
// connection. It does not wait for consumer handlers in flight. A message
// whose handler is still running is redelivered after the ack wait.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	cs := make([]*consumer, 0, len(b.consumers))
	for c := range b.consumers {
		cs = append(cs, c)
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.signal()
	}
	for _, c := range cs {
		<-c.done
	}
	b.nc.Close()
	return nil
}

// Publish implements fw.Bus. A subject that the stream captures goes through
// JetStream and is stored. The Nats-Msg-Id header lets the server drop a
// retried publish of the same event to the same subject. Any other subject
// is a core NATS publish, which reaches subscribers only.
func (b *Bus) Publish(ctx context.Context, subject string, e fw.Event) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("nats: encode event: %w", err)
	}
	m := nats.NewMsg(subject)
	m.Data = data
	if e.Type != "" {
		m.Header.Set(headerType, e.Type)
	}
	if e.Subject != "" {
		m.Header.Set(headerRun, e.Subject)
	}
	if !b.captured(subject) {
		if err := b.nc.PublishMsg(m); err != nil {
			return fmt.Errorf("nats: publish %s: %w", subject, err)
		}
		return nil
	}
	if e.ID != "" {
		m.Header.Set(jetstream.MsgIDHeader, msgID(subject, e.ID))
	}
	if !b.nc.IsConnected() {
		// A stream publish waits for an acknowledgement that cannot come.
		return fmt.Errorf("nats: publish %s: %w", subject, errNotConnected)
	}
	_, err = b.js.PublishMsg(ctx, m)
	if errors.Is(err, jetstream.ErrNoStreamResponse) {
		// The server lost the stream, for example in a restart without
		// storage. Make it again and try once more.
		if eerr := b.ensureStream(ctx, b.currentSubjects()); eerr == nil {
			_, err = b.js.PublishMsg(ctx, m)
		}
	}
	if err != nil {
		return fmt.Errorf("nats: publish %s: %w", subject, err)
	}
	return nil
}

// Subscribe implements fw.Bus with a core NATS subscription. The handler runs
// on one goroutine per subscription, so events arrive in order. A slow handler
// never blocks Publish: NATS drops messages for a subscriber that falls too
// far behind and reports it through the logger. After stop returns, no new
// handler call starts.
func (b *Bus) Subscribe(ctx context.Context, subject string, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("nats: subscribe without a handler")
	}
	if b.isClosed() {
		return nil, errClosed
	}
	var stopped atomic.Bool
	sub, err := b.nc.Subscribe(subject, func(m *nats.Msg) {
		if stopped.Load() {
			return
		}
		var e fw.Event
		if err := json.Unmarshal(m.Data, &e); err != nil {
			b.logger().Warn("bus dropped a message that does not decode", "subject", m.Subject, "error", err)
			return
		}
		if err := h(e); err != nil {
			b.logger().Warn("bus subscriber failed", "subject", m.Subject, "id", e.ID, "type", e.Type, "error", err)
		}
	})
	if err != nil {
		return nil, fmt.Errorf("nats: subscribe %s: %w", subject, err)
	}
	// Flush so that the server knows the subscription before Subscribe
	// returns. While the connection is not up, the client sends the
	// subscription on its own when it connects, and a flush would only wait
	// for the flush timeout.
	if b.nc.IsConnected() {
		if err := flush(ctx, b.nc); err != nil && !b.nc.IsReconnecting() {
			_ = sub.Unsubscribe()
			return nil, fmt.Errorf("nats: subscribe %s: %w", subject, err)
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			stopped.Store(true)
			_ = sub.Unsubscribe()
		})
	}, nil
}

// Consume implements fw.Bus with a durable pull consumer on the stream. The
// durable name comes from the queue, so every process that consumes the same
// queue shares the work. The handler returns nil to acknowledge a message and
// an error to have it delivered again, up to 5 deliveries. Up to the
// concurrency option of messages are handled at a time.
//
// The stop function stops fetching, lets the handlers in flight finish, and
// waits for them. Do not call it from inside the handler.
func (b *Bus) Consume(ctx context.Context, queue string, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("nats: consume without a handler")
	}
	if queue == "" {
		return nil, errors.New("nats: consume without a queue")
	}
	if b.isClosed() {
		return nil, errClosed
	}
	if !b.nc.IsConnected() {
		// The stream and the consumer need the server. Fail at once, so the
		// caller tries again later instead of waiting for a request timeout.
		return nil, fmt.Errorf("nats: consume %s: %w", queue, errNotConnected)
	}
	if err := b.ensureCaptured(ctx, queue); err != nil {
		return nil, err
	}
	cfg := jetstream.ConsumerConfig{
		Durable:       durableName(queue),
		FilterSubject: queue,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       b.ackWait,
		MaxDeliver:    maxDeliver,
	}
	cons, err := b.js.CreateOrUpdateConsumer(ctx, b.stream, cfg)
	if err != nil {
		return nil, fmt.Errorf("nats: consumer %s: %w", cfg.Durable, err)
	}
	c := &consumer{
		b:     b,
		queue: queue,
		cfg:   cfg,
		cons:  cons,
		h:     h,
		sem:   make(chan struct{}, b.concurrency),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errClosed
	}
	b.consumers[c] = struct{}{}
	b.mu.Unlock()
	go c.run()
	return c.stop, nil
}

func (b *Bus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// captured reports whether the stream captures every subject that matches
// the pattern.
func (b *Bus) captured(pattern string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return coveredByAny(b.subjects, pattern)
}

func (b *Bus) currentSubjects() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.subjects...)
}

// ensureCaptured adds a filter to the stream when the stream does not
// capture the pattern yet.
func (b *Bus) ensureCaptured(ctx context.Context, pattern string) error {
	if b.captured(pattern) {
		return nil
	}
	return b.ensureStream(ctx, addFilter(b.currentSubjects(), pattern))
}

// ensureStream creates the stream, or adds the missing subjects to a stream
// that exists. It never removes a subject that another process added.
func (b *Bus) ensureStream(ctx context.Context, want []string) error {
	b.ensureMu.Lock()
	defer b.ensureMu.Unlock()
	s, err := b.js.Stream(ctx, b.stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		_, err = b.js.CreateStream(ctx, jetstream.StreamConfig{
			Name:      b.stream,
			Subjects:  want,
			Storage:   jetstream.FileStorage,
			Retention: jetstream.LimitsPolicy,
		})
		if err == nil {
			b.setSubjects(want)
			return nil
		}
		if !errors.Is(err, jetstream.ErrStreamNameAlreadyInUse) {
			return fmt.Errorf("nats: create stream %s: %w", b.stream, err)
		}
		// Another process created the stream first.
		s, err = b.js.Stream(ctx, b.stream)
	}
	if err != nil {
		return fmt.Errorf("nats: stream %s: %w", b.stream, err)
	}
	cfg := s.CachedInfo().Config
	merged := mergeSubjects(cfg.Subjects, want)
	if len(merged) > len(cfg.Subjects) {
		cfg.Subjects = merged
		if _, err := b.js.UpdateStream(ctx, cfg); err != nil {
			return fmt.Errorf("nats: update stream %s: %w", b.stream, err)
		}
	}
	b.setSubjects(merged)
	return nil
}

func (b *Bus) setSubjects(s []string) {
	b.mu.Lock()
	b.subjects = append([]string(nil), s...)
	b.mu.Unlock()
}

// consumer is one Consume call: a fetch loop and its handlers.
type consumer struct {
	b     *Bus
	queue string
	cfg   jetstream.ConsumerConfig
	cons  jetstream.Consumer // owned by the fetch loop
	h     func(fw.Event) error

	sem      chan struct{} // one slot per handler in flight
	quit     chan struct{}
	quitOnce sync.Once
	done     chan struct{} // closed when the fetch loop ends
	handlers sync.WaitGroup
}

func (c *consumer) signal() { c.quitOnce.Do(func() { close(c.quit) }) }

func (c *consumer) stop() {
	c.signal()
	<-c.done
	c.handlers.Wait()
	c.b.mu.Lock()
	delete(c.b.consumers, c)
	c.b.mu.Unlock()
}

// run fetches as many messages as there are free handler slots. Messages that
// arrive for a pull in flight after stop are still handled: stop drains.
func (c *consumer) run() {
	defer close(c.done)
	failures := 0
	for {
		n, ok := c.acquire()
		if !ok {
			return
		}
		got := 0
		batch, err := c.cons.Fetch(n, jetstream.FetchMaxWait(c.b.fetchWait))
		if err == nil {
			for m := range batch.Messages() {
				got++
				c.handlers.Add(1)
				go c.handle(m)
			}
			err = batch.Error()
		}
		c.release(n - got)
		if err == nil {
			failures = 0
			continue
		}
		failures++
		c.b.logger().Warn("bus fetch failed", "queue", c.queue, "error", err)
		if !c.sleep(backoff(failures)) {
			return
		}
		c.recover()
	}
}

// recover makes the stream and the consumer exist again, for example after a
// server restart that lost its state.
func (c *consumer) recover() {
	ctx, cancel := context.WithTimeout(context.Background(), recoverTimeout)
	defer cancel()
	if err := c.b.ensureStream(ctx, c.b.currentSubjects()); err != nil {
		c.b.logger().Debug("bus stream check failed", "queue", c.queue, "error", err)
		return
	}
	cons, err := c.b.js.CreateOrUpdateConsumer(ctx, c.b.stream, c.cfg)
	if err != nil {
		c.b.logger().Debug("bus consumer check failed", "queue", c.queue, "error", err)
		return
	}
	c.cons = cons
}

func (c *consumer) handle(m jetstream.Msg) {
	defer c.handlers.Done()
	defer c.release(1)
	log := c.b.logger()
	var e fw.Event
	if err := json.Unmarshal(m.Data(), &e); err != nil {
		log.Warn("bus dropped a message that does not decode", "queue", c.queue, "subject", m.Subject(), "error", err)
		_ = m.Term()
		return
	}
	stopBeat := keepAlive(m, c.b.ackWait/3)
	err := c.h(e)
	stopBeat()
	if err == nil {
		if aerr := m.Ack(); aerr != nil {
			log.Warn("bus ack failed", "queue", c.queue, "id", e.ID, "error", aerr)
		}
		return
	}
	var delivered uint64
	if md, merr := m.Metadata(); merr == nil {
		delivered = md.NumDelivered
	}
	if delivered >= maxDeliver {
		log.Error("bus handler failed on the last delivery; message dropped", "queue", c.queue, "id", e.ID, "type", e.Type, "deliveries", delivered, "error", err)
		_ = m.Term()
		return
	}
	log.Warn("bus handler failed; message will be delivered again", "queue", c.queue, "id", e.ID, "type", e.Type, "deliveries", delivered, "error", err)
	if nerr := m.Nak(); nerr != nil {
		log.Warn("bus nak failed", "queue", c.queue, "id", e.ID, "error", nerr)
	}
}

// keepAlive tells the server that a message is in progress until stop is
// called, so that a long handler does not cause a redelivery.
func keepAlive(m jetstream.Msg, every time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = m.InProgress()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// acquire waits for one free handler slot and takes every other free slot.
func (c *consumer) acquire() (int, bool) {
	select {
	case <-c.quit:
		return 0, false
	default:
	}
	select {
	case c.sem <- struct{}{}:
	case <-c.quit:
		return 0, false
	}
	n := 1
	for n < cap(c.sem) {
		select {
		case c.sem <- struct{}{}:
			n++
		default:
			return n, true
		}
	}
	return n, true
}

func (c *consumer) release(n int) {
	for range n {
		<-c.sem
	}
}

func (c *consumer) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.quit:
		return false
	}
}

func backoff(failures int) time.Duration {
	d := 250 * time.Millisecond
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func flush(ctx context.Context, nc *nats.Conn) error {
	if _, ok := ctx.Deadline(); ok {
		return nc.FlushWithContext(ctx)
	}
	return nc.FlushTimeout(flushTimeout)
}

// msgID is the deduplication id of an event on a subject. It names the
// subject so that the same event sent to two subjects is stored twice.
func msgID(subject, id string) string { return id + "@" + subject }

// durableName maps a queue to a consumer name. Consumer names cannot hold
// ".", "*", ">", path separators, or white space.
func durableName(queue string) string {
	var sb strings.Builder
	sb.WriteString("fw-")
	for _, r := range queue {
		switch {
		case r == '.':
			sb.WriteByte('-')
		case r == '*':
			sb.WriteByte('_')
		case r == '>':
			sb.WriteString("__")
		case r <= ' ' || r == 0x7f || r == '/' || r == '\\':
			sb.WriteByte('_')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// streamSubjects returns the stream filters: the list from the subjects
// option, plus a filter for the intake queue and the egress prefix when the
// list does not capture them.
func streamSubjects(list, intake, egress string) []string {
	var subs []string
	for _, s := range strings.Split(list, ",") {
		if s = strings.TrimSpace(s); s != "" {
			subs = append(subs, s)
		}
	}
	if len(subs) == 0 {
		subs = []string{defaultSubjects}
	}
	if intake != "" {
		subs = addFilter(subs, intake)
	}
	if egress != "" {
		subs = addFilter(subs, egress+".>")
	}
	return subs
}

// addFilter adds the first token of the pattern followed by ">" when no
// filter captures the pattern.
func addFilter(subs []string, pattern string) []string {
	if coveredByAny(subs, pattern) {
		return subs
	}
	return append(subs, prefixFilter(pattern))
}

func prefixFilter(pattern string) string {
	first, _, more := strings.Cut(pattern, ".")
	if !more || first == "*" || first == ">" {
		return pattern
	}
	return first + ".>"
}

// mergeSubjects returns have plus each subject of want that have does not
// capture.
func mergeSubjects(have, want []string) []string {
	out := append([]string(nil), have...)
	for _, w := range want {
		if !coveredByAny(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func coveredByAny(filters []string, pattern string) bool {
	for _, f := range filters {
		if covers(f, pattern) {
			return true
		}
	}
	return false
}

// covers reports whether every subject that matches pattern also matches
// filter. Both use NATS wildcards: "*" is one token and ">" is the rest.
func covers(filter, pattern string) bool {
	ft := strings.Split(filter, ".")
	pt := strings.Split(pattern, ".")
	for i, f := range ft {
		if f == ">" {
			return i < len(pt)
		}
		if i >= len(pt) || pt[i] == ">" {
			return false
		}
		if f != "*" && f != pt[i] {
			return false
		}
	}
	return len(ft) == len(pt)
}

// checkSubject rejects a subject that cannot be published to.
func checkSubject(subject string) error {
	if subject == "" {
		return errors.New("nats: publish without a subject")
	}
	for _, t := range strings.Split(subject, ".") {
		if t == "" || t == "*" || t == ">" {
			return fmt.Errorf("nats: cannot publish to %q: it needs non-empty tokens and no wildcard", subject)
		}
	}
	return nil
}

func intOption(opts map[string]string, key string, def int) (int, error) {
	v := strings.TrimSpace(opts[key])
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("nats: option %s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// redact drops the URL from a parse error, because the URL can hold a
// password or a token.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

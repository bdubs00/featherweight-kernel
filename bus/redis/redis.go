// Package redis is the Redis bus driver. It registers itself as "redis".
//
// Each subject is one Redis stream with the same key, and one Pub/Sub
// channel with the same name. Publish adds the event to both. Subscribe
// listens on the channels: fan-out with no durability. Consume reads the
// stream through a consumer group with at-least-once delivery. ConsumeGated
// is Consume with a fw.Gate, which the bus intake of the kernel uses to take
// a job only for a free run slot.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() {
	fw.RegisterBus("redis", func(ctx context.Context, cfg *fw.Config) (fw.Bus, error) {
		b, err := Open(ctx, cfg)
		if err != nil {
			return nil, err
		}
		return b, nil
	})
}

const (
	defaultURL    = "redis://127.0.0.1:6379/0"
	defaultGroup  = "fw"
	defaultMaxLen = 100000

	maxDeliver        = 5
	defaultBlock      = 2 * time.Second
	defaultClaimEvery = time.Minute
	// defaultClaimIdle is long because a handler of Consume can work for a
	// long time. A handler that runs longer still resets the idle time. The
	// bus intake of the kernel acknowledges a job when its run is in the
	// ledger, which takes milliseconds.
	defaultClaimIdle = 5 * time.Minute
	claimBatch       = 100
	subBuffer        = 1024
	maxBackoff       = 5 * time.Second

	fieldEvent = "event"
	fieldType  = "type"
	fieldRun   = "run"
)

var errClosed = errors.New("redis: bus is closed")

// Bus is a fw.Bus on Redis streams and Pub/Sub.
type Bus struct {
	rdb         *redis.Client
	group       string
	maxlen      int64
	concurrency int
	name        string // the consumer name: host name and process id
	log         atomic.Pointer[slog.Logger]

	// Tests shorten these.
	block      time.Duration // XREADGROUP BLOCK; stop waits at most this long
	claimEvery time.Duration // how often to look for messages to deliver again
	claimIdle  time.Duration // how long a failed message waits before it is delivered again

	mu        sync.Mutex
	consumers map[*consumer]struct{}
	subs      map[*redis.PubSub]struct{}
	closed    bool
}

// Open connects to Redis.
//
// It reads cfg.Bus.URL (redis://host:port/db, default
// redis://127.0.0.1:6379/0) and these keys of cfg.Bus.Options:
//
//   - group: the consumer group name (default fw)
//   - maxlen: the approximate cap on the length of each stream (default
//     100000)
//   - concurrency: the number of messages that one Consume handles at a time
//     (default: the run slots of the agent, fw.Config.RunSlots, which is 1
//     under ax)
//
// Each subject is a stream of its own, so the dead-letter subject of the
// intake keeps its records with no setting.
func Open(ctx context.Context, cfg *fw.Config) (*Bus, error) {
	if cfg == nil {
		return nil, errors.New("redis: no config")
	}
	opt := cfg.Bus.Options
	conc, err := intOption(opt, "concurrency", cfg.RunSlots())
	if err != nil {
		return nil, err
	}
	maxlen, err := intOption(opt, "maxlen", defaultMaxLen)
	if err != nil {
		return nil, err
	}
	group := strings.TrimSpace(opt["group"])
	if group == "" {
		group = defaultGroup
	}
	raw := cfg.Bus.URL
	if raw == "" {
		raw = defaultURL
	}
	ropt, err := redis.ParseURL(raw)
	if err != nil {
		return nil, fmt.Errorf("redis: bus url: %w", redact(err))
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "fw"
	}
	b := &Bus{
		rdb:         redis.NewClient(ropt),
		group:       group,
		maxlen:      int64(maxlen),
		concurrency: conc,
		name:        host + "-" + strconv.Itoa(os.Getpid()),
		block:       defaultBlock,
		claimEvery:  defaultClaimEvery,
		claimIdle:   defaultClaimIdle,
		consumers:   map[*consumer]struct{}{},
		subs:        map[*redis.PubSub]struct{}{},
	}
	b.SetLogger(slog.Default())
	// A server that is not reachable at start does not stop the process.
	// Under ax, the sandbox has no egress until the process reports ready.
	// The client connects again at each command.
	if err := b.rdb.Ping(ctx).Err(); err != nil {
		b.logger().Warn("bus not reachable yet; the client connects again at each command", "error", redact(err))
	}
	return b, nil
}

// SetLogger sets the logger for handler failures. The default is
// slog.Default(). Log lines carry ids, types, and subjects, never event data.
func (b *Bus) SetLogger(l *slog.Logger) {
	if l != nil {
		b.log.Store(l.With("bus", "redis"))
	}
}

func (b *Bus) logger() *slog.Logger { return b.log.Load() }

// Close stops every consumer loop, subscription, and watch and closes the
// client. It does not wait for handlers in flight. A message whose handler is still
// running stays pending and is delivered again after the claim idle time.
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
	ps := make([]*redis.PubSub, 0, len(b.subs))
	for p := range b.subs {
		ps = append(ps, p)
	}
	b.mu.Unlock()
	for _, c := range cs {
		c.signal()
	}
	for _, c := range cs {
		c.loops.Wait()
	}
	for _, p := range ps {
		_ = p.Close()
	}
	return b.rdb.Close()
}

// Publish implements fw.Bus. It adds the event to the stream named by the
// subject, with the fields "event" (the JSON), "type", and "run", and
// publishes the JSON on the channel with the same name. A subscriber never
// slows Publish: Redis buffers Pub/Sub output per client.
func (b *Bus) Publish(ctx context.Context, subject string, e fw.Event) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("redis: encode event: %w", err)
	}
	_, err = b.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		p.XAdd(ctx, &redis.XAddArgs{
			Stream: subject,
			MaxLen: b.maxlen,
			Approx: true,
			Values: []any{fieldEvent, data, fieldType, e.Type, fieldRun, e.Subject},
		})
		p.Publish(ctx, subject, data)
		return nil
	})
	if err != nil {
		return fmt.Errorf("redis: publish %s: %w", subject, err)
	}
	return nil
}

// Subscribe implements fw.Bus. A subject with a wildcard becomes a PSUBSCRIBE
// pattern, and each message is checked against the subject with
// fw.SubjectMatch, because a Redis "*" also matches dots. The handler runs on
// one goroutine per subscription, so events arrive in order. After stop
// returns, no new handler call starts.
func (b *Bus) Subscribe(ctx context.Context, subject string, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("redis: subscribe without a handler")
	}
	if subject == "" {
		return nil, errors.New("redis: subscribe without a subject")
	}
	if b.isClosed() {
		return nil, errClosed
	}
	var ps *redis.PubSub
	if hasWildcard(subject) {
		ps = b.rdb.PSubscribe(ctx, globPattern(subject))
	} else {
		ps = b.rdb.Subscribe(ctx, subject)
	}
	// Wait for the confirmation so that the subscription is active when
	// Subscribe returns.
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("redis: subscribe %s: %w", subject, err)
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		_ = ps.Close()
		return nil, errClosed
	}
	b.subs[ps] = struct{}{}
	b.mu.Unlock()

	var stopped atomic.Bool
	ch := ps.Channel(redis.WithChannelSize(subBuffer))
	go func() {
		for m := range ch {
			if stopped.Load() || !fw.SubjectMatch(subject, m.Channel) {
				continue
			}
			var e fw.Event
			if err := json.Unmarshal([]byte(m.Payload), &e); err != nil {
				b.logger().Warn("bus dropped a message that does not decode", "subject", m.Channel, "error", err)
				continue
			}
			if err := h(e); err != nil {
				b.logger().Warn("bus subscriber failed", "subject", m.Channel, "id", e.ID, "type", e.Type, "error", err)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			stopped.Store(true)
			b.mu.Lock()
			delete(b.subs, ps)
			b.mu.Unlock()
			_ = ps.Close()
		})
	}, nil
}

// Consume implements fw.Bus with a consumer group on the stream named by the
// queue. The queue is an exact subject: Redis has one stream per subject.
// The handler returns nil to acknowledge a message. On an error the message
// stays pending. A periodic claim delivers it again once it has been idle
// for the claim idle time, up to 5 deliveries. Up to the concurrency option
// of messages are handled at a time.
//
// The stop function stops reading, lets the handlers in flight finish, and
// waits for them. Do not call it from inside the handler.
func (b *Bus) Consume(ctx context.Context, queue string, h func(fw.Event) error) (func(), error) {
	return b.consume(ctx, queue, nil, h)
}

// ConsumeGated implements fw.GatedBus. It is Consume, and before each read
// it asks the gate how many messages to take: at most the free handler
// slots, and at least one, so the loop waits in Admit while the gate has
// no room. The claim loop takes an admission for each message that it
// delivers again. The messages that it does not take stay in the stream
// for the other consumers of the group. The stop function ends a wait in
// Admit.
func (b *Bus) ConsumeGated(ctx context.Context, queue string, g fw.Gate, h func(fw.Event) error) (func(), error) {
	if g == nil {
		return nil, errors.New("redis: consume without a gate")
	}
	return b.consume(ctx, queue, g, h)
}

func (b *Bus) consume(ctx context.Context, queue string, g fw.Gate, h func(fw.Event) error) (func(), error) {
	if h == nil {
		return nil, errors.New("redis: consume without a handler")
	}
	if queue == "" || hasWildcard(queue) {
		return nil, fmt.Errorf("redis: queue %q must be an exact subject", queue)
	}
	if b.isClosed() {
		return nil, errClosed
	}
	if err := b.ensureGroup(ctx, queue); err != nil {
		return nil, err
	}
	qctx, cancel := context.WithCancel(context.Background())
	c := &consumer{
		b:      b,
		queue:  queue,
		h:      h,
		gate:   g,
		sem:    make(chan struct{}, b.concurrency),
		quit:   make(chan struct{}),
		qctx:   qctx,
		cancel: cancel,
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		cancel()
		return nil, errClosed
	}
	b.consumers[c] = struct{}{}
	b.mu.Unlock()
	c.loops.Add(2)
	go c.read()
	go c.claim()
	return c.stop, nil
}

func (b *Bus) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

// ensureGroup creates the stream and the consumer group. The group starts at
// the beginning of the stream, so events published before the first
// consumer are not lost.
func (b *Bus) ensureGroup(ctx context.Context, queue string) error {
	err := b.rdb.XGroupCreateMkStream(ctx, queue, b.group, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("redis: create group %s on %s: %w", b.group, queue, err)
	}
	return nil
}

// consumer is one Consume call: a read loop, a claim loop, and handlers.
type consumer struct {
	b     *Bus
	queue string
	h     func(fw.Event) error
	gate  fw.Gate // nil for Consume
	// turn makes the read loop and the claim loop take turns at the gate.
	// The read loop asks again as soon as a read comes back empty, so
	// without turns it keeps every admission from the claim loop.
	turn sync.Mutex

	sem      chan struct{} // one slot per handler in flight
	quit     chan struct{}
	quitOnce sync.Once
	qctx     context.Context // ends with quit, for a wait in the gate
	cancel   context.CancelFunc
	loops    sync.WaitGroup // the read and claim loops
	handlers sync.WaitGroup
}

func (c *consumer) signal() {
	c.quitOnce.Do(func() {
		close(c.quit)
		c.cancel()
	})
}

// admit asks the gate for room for up to n messages, for which the caller
// holds n handler slots. It gives back the slots that the gate does not
// admit and returns the number admitted. Without a gate it admits n. It
// returns false when the stop or the owner of the gate ends the wait.
func (c *consumer) admit(n int) (int, bool) {
	if c.gate == nil {
		return n, true
	}
	c.turn.Lock()
	admitted, err := c.gate.Admit(c.qctx, n)
	c.turn.Unlock()
	if err != nil {
		c.release(n)
		return 0, false
	}
	admitted = min(admitted, n)
	c.release(n - admitted)
	return admitted, true
}

// unused gives back n admissions that got no message.
func (c *consumer) unused(n int) {
	if c.gate != nil && n > 0 {
		c.gate.Unused(n)
	}
}

func (c *consumer) stop() {
	c.signal()
	c.loops.Wait()
	c.handlers.Wait()
	c.b.mu.Lock()
	delete(c.b.consumers, c)
	c.b.mu.Unlock()
}

// read reads as many new messages as there are free handler slots, and
// with a gate no more than the gate admits. Messages that a read in flight
// returns after stop are still handled: stop drains.
func (c *consumer) read() {
	defer c.loops.Done()
	ctx := context.Background()
	failures := 0
	for {
		n, ok := c.acquire()
		if !ok {
			return
		}
		if n, ok = c.admit(n); !ok {
			return
		}
		got := 0
		res, err := c.b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.b.group,
			Consumer: c.b.name,
			Streams:  []string{c.queue, ">"},
			Count:    int64(n),
			Block:    c.b.block,
		}).Result()
		if err == nil {
			for _, s := range res {
				for _, m := range s.Messages {
					got++
					c.dispatch(m)
				}
			}
		}
		c.release(n - got)
		c.unused(n - got)
		if err == nil || errors.Is(err, redis.Nil) {
			failures = 0
			continue
		}
		if errors.Is(err, redis.ErrClosed) {
			return
		}
		failures++
		c.b.logger().Warn("bus read failed", "queue", c.queue, "error", err)
		if !c.sleep(backoff(failures)) {
			return
		}
		if isNoGroup(err) {
			if err := c.b.ensureGroup(ctx, c.queue); err != nil {
				c.b.logger().Debug("bus group check failed", "queue", c.queue, "error", err)
			}
		}
	}
}

// claim delivers again the messages that stayed pending longer than the
// claim idle time: messages whose handler failed, and messages of a consumer
// that died. A message delivered maxDeliver times is acknowledged and logged
// instead.
func (c *consumer) claim() {
	defer c.loops.Done()
	t := time.NewTicker(c.b.claimEvery)
	defer t.Stop()
	for {
		select {
		case <-c.quit:
			return
		case <-t.C:
		}
		c.reclaim()
	}
}

func (c *consumer) reclaim() {
	ctx := context.Background()
	log := c.b.logger()
	pending, err := c.b.rdb.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.queue,
		Group:  c.b.group,
		Idle:   c.b.claimIdle,
		Start:  "-",
		End:    "+",
		Count:  claimBatch,
	}).Result()
	if err != nil {
		if !errors.Is(err, redis.ErrClosed) {
			log.Debug("bus pending check failed", "queue", c.queue, "error", err)
		}
		return
	}
	for _, p := range pending {
		if p.RetryCount >= maxDeliver {
			if err := c.b.rdb.XAck(ctx, c.queue, c.b.group, p.ID).Err(); err != nil {
				log.Warn("bus ack failed", "queue", c.queue, "entry", p.ID, "error", err)
				continue
			}
			log.Error("bus handler failed on the last delivery; message dropped", "queue", c.queue, "entry", p.ID, "deliveries", p.RetryCount)
			continue
		}
		if !c.acquireOne() {
			return
		}
		if _, ok := c.admit(1); !ok {
			return
		}
		// XCLAIM checks the idle time again, so only one consumer wins.
		msgs, err := c.b.rdb.XClaim(ctx, &redis.XClaimArgs{
			Stream:   c.queue,
			Group:    c.b.group,
			Consumer: c.b.name,
			MinIdle:  c.b.claimIdle,
			Messages: []string{p.ID},
		}).Result()
		if err != nil || len(msgs) == 0 {
			c.release(1)
			c.unused(1)
			continue
		}
		if msgs[0].Values == nil {
			// The stream cap removed the entry.
			_ = c.b.rdb.XAck(ctx, c.queue, c.b.group, p.ID).Err()
			c.release(1)
			c.unused(1)
			continue
		}
		c.dispatch(msgs[0])
	}
}

// dispatch hands a message to a handler. The caller holds a slot for it.
func (c *consumer) dispatch(m redis.XMessage) {
	c.handlers.Add(1)
	go c.handle(m)
}

func (c *consumer) handle(m redis.XMessage) {
	defer c.handlers.Done()
	defer c.release(1)
	log := c.b.logger()
	raw, _ := m.Values[fieldEvent].(string)
	var e fw.Event
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		log.Warn("bus dropped a message that does not decode", "queue", c.queue, "entry", m.ID, "error", err)
		c.ack(m.ID, "")
		// The handler never saw the message, so its admission comes back here.
		c.unused(1)
		return
	}
	stopBeat := c.keepAlive(m.ID)
	err := c.h(e)
	stopBeat()
	if err == nil {
		c.ack(m.ID, e.ID)
		return
	}
	log.Warn("bus handler failed; message will be delivered again", "queue", c.queue, "entry", m.ID, "id", e.ID, "type", e.Type, "error", err)
}

func (c *consumer) ack(entry, id string) {
	err := c.b.rdb.XAck(context.Background(), c.queue, c.b.group, entry).Err()
	if err != nil {
		c.b.logger().Warn("bus ack failed", "queue", c.queue, "entry", entry, "id", id, "error", err)
	}
}

// keepAlive claims the message for this consumer again and again until stop
// is called. Each claim resets the idle time, so a long handler does not
// look failed. JUSTID keeps the delivery count.
func (c *consumer) keepAlive(entry string) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(c.b.claimIdle / 3)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				_ = c.b.rdb.XClaimJustID(context.Background(), &redis.XClaimArgs{
					Stream:   c.queue,
					Group:    c.b.group,
					Consumer: c.b.name,
					Messages: []string{entry},
				}).Err()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}

// acquire waits for one free handler slot and takes every other free slot.
func (c *consumer) acquire() (int, bool) {
	if !c.acquireOne() {
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

func (c *consumer) acquireOne() bool {
	select {
	case <-c.quit:
		return false
	default:
	}
	select {
	case c.sem <- struct{}{}:
		return true
	case <-c.quit:
		return false
	}
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

func isNoGroup(err error) bool { return strings.HasPrefix(err.Error(), "NOGROUP") }

// hasWildcard reports whether a NATS-style subject has a "*" or ">" token.
func hasWildcard(subject string) bool {
	for _, t := range strings.Split(subject, ".") {
		if t == "*" || t == ">" {
			return true
		}
	}
	return false
}

// globPattern maps a NATS-style subject to a Redis glob pattern: "*" and ">"
// become "*", and glob characters in other tokens are escaped. A Redis "*"
// also matches dots, so the pattern is wider than the subject, and the
// subscriber filters with fw.SubjectMatch.
func globPattern(subject string) string {
	toks := strings.Split(subject, ".")
	for i, t := range toks {
		if t == "*" || t == ">" {
			toks[i] = "*"
			continue
		}
		toks[i] = escapeGlob(t)
	}
	return strings.Join(toks, ".")
}

func escapeGlob(s string) string {
	if !strings.ContainsAny(s, `*?[]\`) {
		return s
	}
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case '*', '?', '[', ']', '\\':
			sb.WriteByte('\\')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// checkSubject rejects a subject that is not one exact stream key.
func checkSubject(subject string) error {
	if subject == "" {
		return errors.New("redis: empty subject")
	}
	if hasWildcard(subject) {
		return fmt.Errorf("redis: subject %q has a wildcard", subject)
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
		return 0, fmt.Errorf("redis: option %s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// redact drops the URL from a parse error, because the URL can hold a
// password.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

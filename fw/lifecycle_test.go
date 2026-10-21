package fw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type acceptanceLedger struct {
	Ledger
	entered    chan struct{}
	release    chan struct{}
	err        error
	writes     atomic.Int32
	parentOnly bool
}

func (l *acceptanceLedger) Append(ctx context.Context, e Event) (uint64, error) {
	if e.Type == EvRunQueued {
		if l.parentOnly {
			var q RunQueuedData
			_ = e.Decode(&q)
			if q.Parent == "" {
				return l.Ledger.Append(ctx, e)
			}
		}
		if l.writes.Add(1) == 1 {
			close(l.entered)
		}
		<-l.release
		if l.err != nil {
			return 0, l.err
		}
	}
	return l.Ledger.Append(ctx, e)
}

func TestStartAcceptanceWaiters(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			ctx := testCtx(t)
			l := &acceptanceLedger{Ledger: NewMemLedger(), entered: make(chan struct{}), release: make(chan struct{})}
			cause := errors.New("injected initial append failure")
			if fail {
				l.err = cause
			}
			a := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(l))
			type result struct {
				r   *Run
				err error
			}
			owner := make(chan result, 1)
			go func() { r, err := a.Start(ctx, "work", WithIdempotencyKey("same")); owner <- result{r, err} }()
			<-l.entered
			// Cancellation belongs to the waiter, not the owner of acceptance.
			cctx, cancel := context.WithCancel(ctx)
			cancel()
			if r, err := a.Start(cctx, "work", WithIdempotencyKey("same")); r != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled retry = %v, %v", r, err)
			}
			const n = 32
			results := make(chan result, n)
			var ready sync.WaitGroup
			ready.Add(n)
			for range n {
				go func() {
					ready.Done()
					r, err := a.Start(ctx, "work", WithIdempotencyKey("same"))
					results <- result{r, err}
				}()
			}
			ready.Wait()
			select {
			case r := <-results:
				t.Fatalf("retry returned before acceptance: %+v", r)
			case <-time.After(20 * time.Millisecond):
			}
			close(l.release)
			first := <-owner
			for range n {
				got := <-results
				if fail {
					if got.r != nil || !errors.Is(got.err, cause) || got.err != first.err {
						t.Fatalf("retry failure = %+v, owner = %+v", got, first)
					}
				} else if got.err != nil || first.err != nil || got.r.ID != first.r.ID {
					t.Fatalf("retry = %+v, owner = %+v", got, first)
				}
			}
			if l.writes.Load() != 1 {
				t.Fatalf("initial writes = %d", l.writes.Load())
			}
			if !fail {
				r, err := a.Wait(ctx, first.r.ID)
				mustState(t, r, err, StateCompleted)
			}
		})
	}
}

type eventFaultLedger struct {
	Ledger
	typ    string
	after  bool
	cause  error
	failed atomic.Bool
	later  atomic.Int32
}

func (l *eventFaultLedger) Append(ctx context.Context, e Event) (uint64, error) {
	if l.failed.Load() {
		l.later.Add(1)
	}
	if e.Type == l.typ && l.failed.CompareAndSwap(false, true) {
		if l.after {
			if _, err := l.Ledger.Append(ctx, e); err != nil {
				return 0, err
			}
		}
		return 0, l.cause
	}
	return l.Ledger.Append(ctx, e)
}

func TestStorageFailureFailStop(t *testing.T) {
	for _, typ := range []string{EvRunQueued, EvRunStarted, EvModelRequested, EvModelResponded, EvToolRequested, EvToolResponded, EvRunFinished} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/ambiguous=%v", typ, after), func(t *testing.T) {
				ctx := testCtx(t)
				cause := errors.New("injected write or fsync fault")
				l := &eventFaultLedger{Ledger: NewMemLedger(), typ: typ, after: after, cause: cause}
				var effects atomic.Int32
				tool := NewTool(ToolSpec{Name: "effect", ReadOnly: true}, func(context.Context, json.RawMessage) (ToolResult, error) {
					effects.Add(1)
					return TextResult("effect done"), nil
				})
				s := newScript(callTools(use("c1", "effect", `{}`)), endTurn("done"))
				a := newTestAgent(t, s.Model(), WithLedger(l), WithTools(tool))
				r, err := a.Start(ctx, "work", WithIdempotencyKey("same"))
				if err == nil {
					r, err = a.Wait(ctx, r.ID)
				}
				if !errors.Is(err, ErrStorageUnavailable) || !errors.Is(err, cause) || !strings.Contains(err.Error(), typ) {
					t.Fatalf("failure = %v", err)
				}
				if r != nil && (r.Blocked == "" || r.State.Terminal()) {
					t.Fatalf("runtime status = %+v", r)
				}
				a.SetReady(true)
				if a.Ready() || !errors.Is(a.RuntimeError(), cause) {
					t.Fatalf("healthy after failure: %v", a.RuntimeError())
				}
				h, err := a.Handler()
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
				if w.Code != 503 || !strings.Contains(w.Body.String(), "restart") {
					t.Fatalf("readiness = %d %s", w.Code, w.Body.String())
				}
				if _, err := a.Start(ctx, "later"); !errors.Is(err, cause) {
					t.Fatalf("late start = %v", err)
				}
				if r != nil {
					if err := a.Send(ctx, r.ID, Message{Content: []Block{Text("again")}}, OriginHTTP); !errors.Is(err, cause) {
						t.Fatalf("late send = %v", err)
					}
					got, err := a.Get(ctx, r.ID)
					if err != nil || got.Blocked == "" {
						t.Fatalf("Get = %+v, %v", got, err)
					}
				}
				want := int32(0)
				if typ == EvToolResponded || typ == EvRunFinished {
					want = 1
				}
				if effects.Load() != want {
					t.Fatalf("effects = %d, want %d", effects.Load(), want)
				}
				cctx, cancel := context.WithTimeout(ctx, time.Second)
				a.Close(cctx)
				cancel()
				if l.later.Load() != 0 {
					t.Fatalf("writes after failure = %d", l.later.Load())
				}
			})
		}
	}
}

type readFaultLedger struct {
	Ledger
	cause error
	armed atomic.Bool
}

func (l *readFaultLedger) Append(ctx context.Context, e Event) (uint64, error) {
	n, err := l.Ledger.Append(ctx, e)
	if e.Type == EvRunQueued {
		l.armed.Store(true)
	}
	return n, err
}

func (l *readFaultLedger) Read(ctx context.Context, id string, from uint64) iter.Seq2[Event, error] {
	if l.armed.Load() {
		return func(yield func(Event, error) bool) { yield(Event{}, l.cause) }
	}
	return l.Ledger.Read(ctx, id, from)
}

func TestStorageReadFailureBlocksDriver(t *testing.T) {
	ctx := testCtx(t)
	cause := errors.New("injected ledger read fault")
	l := &readFaultLedger{Ledger: NewMemLedger(), cause: cause}
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithLedger(l))
	r, err := a.Start(ctx, "work")
	if err != nil && !errors.Is(err, cause) {
		t.Fatal(err)
	}
	if err == nil {
		_, err = a.Wait(ctx, r.ID)
	}
	if !errors.Is(err, cause) || a.Ready() {
		t.Fatalf("read failure = %v, ready=%v", err, a.Ready())
	}
}

func TestStorageFailureRestartNeverRepeatsUnsafeEffect(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(fmt.Sprintf("resultStored=%v", after), func(t *testing.T) {
			ctx := testCtx(t)
			base := NewMemLedger()
			l := &eventFaultLedger{Ledger: base, typ: EvToolResponded, after: after, cause: errors.New("injected result fsync failure")}
			var effects atomic.Int32
			tool := NewTool(ToolSpec{Name: "effect", ReadOnly: true, Idempotent: false}, func(context.Context, json.RawMessage) (ToolResult, error) {
				effects.Add(1)
				return TextResult("effect done"), nil
			})
			a := newTestAgent(t, newScript(callTools(use("c1", "effect", `{}`))).Model(), WithLedger(l), WithTools(tool))
			r, err := a.Start(ctx, "work")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Wait(ctx, r.ID); !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("first failure = %v", err)
			}
			a.Close(ctx)
			resumed := newTestAgent(t, newScript(endTurn("done")).Model(), WithLedger(base), WithTools(tool))
			got, err := resumed.Wait(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := StateInputRequired
			if after {
				want = StateCompleted
			}
			if got.State != want || got.Blocked != "" || effects.Load() != 1 {
				t.Fatalf("restart state=%s blocked=%q effects=%d, want state=%s effects=1", got.State, got.Blocked, effects.Load(), want)
			}
		})
	}
}

func TestDirectAdmissionBoundConcurrent(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 3
	a := newTestAgent(t, blockingModel(make(chan struct{}, 128)), WithConfig(cfg))
	var accepted atomic.Int32
	var rejected atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Start(ctx, "work")
			if err == nil {
				accepted.Add(1)
				return
			}
			var full *SaturationError
			if !errors.As(err, &full) || !errors.Is(err, ErrSaturated) || full.MaxQueuedRuns != 3 {
				t.Errorf("start = %v", err)
			}
			rejected.Add(1)
		}()
	}
	wg.Wait()
	if accepted.Load() != 4 || rejected.Load() != 196 {
		t.Fatalf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	all, err := a.List(ctx)
	if err != nil || len(all) != 4 {
		t.Fatalf("retained=%d, err=%v", len(all), err)
	}
	a.k.slotMu.Lock()
	n := a.k.rootDrivers
	a.k.slotMu.Unlock()
	if n != 4 {
		t.Fatalf("root admissions=%d", n)
	}
	for _, r := range all {
		if err := a.Cancel(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDirectAdmissionRetryAndChildBypass(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	a := newTestAgent(t, blockingModel(make(chan struct{}, 16)), WithConfig(cfg))
	r, err := a.Start(ctx, "parent", WithIdempotencyKey("same"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, "queued"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Start(ctx, "full"); !errors.Is(err, ErrSaturated) {
		t.Fatalf("full = %v", err)
	}
	got, err := a.Start(ctx, "parent", WithIdempotencyKey("same"))
	if err != nil || got.ID != r.ID {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	if _, err := a.Start(ctx, "child", WithParent(r.ID)); err != nil {
		t.Fatalf("child = %v", err)
	}
	for _, rs := range a.k.all() {
		if err := a.Cancel(ctx, rs.id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartChildRetryWaitsForAcceptance(t *testing.T) {
	ctx := testCtx(t)
	l := &acceptanceLedger{Ledger: NewMemLedger(), entered: make(chan struct{}), release: make(chan struct{}), parentOnly: true}
	a := newTestAgent(t, newScript(endTurn("parent done"), endTurn("child done")).Model(), WithLedger(l))
	parent, err := a.Run(ctx, "parent")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 2)
	go func() {
		_, err := a.Start(ctx, "child", WithParent(parent.ID), WithIdempotencyKey("child"))
		result <- err
	}()
	<-l.entered
	go func() {
		_, err := a.Start(ctx, "child", WithParent(parent.ID), WithIdempotencyKey("child"))
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("child start returned early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(l.release)
	for range 2 {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if l.writes.Load() != 1 {
		t.Fatalf("child writes = %d", l.writes.Load())
	}
}

// A shutdown waits for an initial append in flight. Once fw.run.queued is
// in the ledger, the run is accepted: the start returns it, and the
// shutdown suspends it, so the next start of the process resumes it.
func TestStartAcceptanceAndShutdown(t *testing.T) {
	ctx := testCtx(t)
	l := &acceptanceLedger{Ledger: NewMemLedger(), entered: make(chan struct{}), release: make(chan struct{})}
	a := newTestAgent(t, newScript(endTurn("unused")).Model(), WithLedger(l))
	type result struct {
		r   *Run
		err error
	}
	owner := make(chan result, 1)
	go func() { r, err := a.Start(ctx, "work", WithIdempotencyKey("same")); owner <- result{r, err} }()
	<-l.entered
	closed := make(chan struct{})
	go func() { a.Close(ctx); close(closed) }()
	<-a.k.drainCh
	select {
	case <-closed:
		t.Fatal("shutdown passed in-flight durable acceptance")
	default:
	}
	close(l.release)
	got := <-owner
	if got.err != nil || got.r == nil || got.r.State != StateQueued {
		t.Fatalf("owner during shutdown = %+v, %v", got.r, got.err)
	}
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if types := eventTypes(ledgerEvents(t, l.Ledger, got.r.ID)); len(types) != 2 || types[0] != EvRunQueued || types[1] != EvRunSuspended {
		t.Fatalf("ledger of the accepted run = %v", types)
	}
	a.k.slotMu.Lock()
	n := a.k.rootDrivers
	a.k.slotMu.Unlock()
	if n != 0 {
		t.Fatalf("admissions after shutdown = %d", n)
	}
}

func TestDirectAdmissionReservationsIncludeBlockedAppend(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.MaxRuns, cfg.MaxQueuedRuns = 1, 1
	l := &acceptanceLedger{Ledger: NewMemLedger(), entered: make(chan struct{}), release: make(chan struct{})}
	a := newTestAgent(t, blockingModel(make(chan struct{}, 16)), WithConfig(cfg), WithLedger(l))
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := a.Start(ctx, "work"); results <- err }()
	}
	<-l.entered
	// The second append can reserve concurrently; wait for both entries.
	for l.writes.Load() != 2 {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := a.Start(ctx, "full"); !errors.Is(err, ErrSaturated) {
		t.Fatalf("full before writes finish = %v", err)
	}
	close(l.release)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewFailureReleasesOwnedLedger(t *testing.T) {
	for _, stage := range []string{"bus", "authn", "authz", "external", "tls", "rebuild", "service"} {
		t.Run(stage, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Home = t.TempDir()
			cfg.Ledger.Path = filepath.Join(cfg.Home, "ledger")
			cfg.Tools.Files.Root = cfg.Home
			opts := []Option{WithConfig(cfg), WithoutDefaults(), WithLogger(quietLogger())}
			switch stage {
			case "bus":
				cfg.Bus.Driver = "missing-setup-failure-driver"
			case "authn":
				cfg.Auth.Mode = "missing-setup-failure-authn"
			case "authz":
				opts = append(opts, WithAuthzFrom(func(*Agent) (Authz, string, error) { return nil, "", errors.New("injected authz setup failure") }))
			case "external":
				cfg.Policy.External = &ExternalPolicyConfig{}
			case "tls":
				cfg.TLS.Provider = "missing-setup-failure-tls"
			case "rebuild":
				l, err := OpenFileLedger(cfg.Ledger.Path)
				if err != nil {
					t.Fatal(err)
				}
				id := NewID()
				if _, err := l.Append(context.Background(), MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "corrupt after close"})); err != nil {
					t.Fatal(err)
				}
				if err := l.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.Ledger.Path, id, "ledger.jsonl"), []byte("not-json\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "service":
				opts = append(opts, WithServices(func(context.Context, *Agent) (io.Closer, error) {
					return nil, errors.New("injected service setup failure")
				}))
			}
			if a, err := New(opts...); err == nil || a != nil {
				t.Fatalf("New = %v, %v", a, err)
			}
			l, err := OpenFileLedger(cfg.Ledger.Path)
			if err != nil {
				t.Fatalf("setup failure leaked writer ownership: %v", err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNewFailureLeavesCallerLedgerOpen(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Files.Root = cfg.Home
	cfg.Bus.Driver = "missing-setup-failure-driver"
	l, err := OpenFileLedger(filepath.Join(cfg.Home, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := New(WithConfig(cfg), WithoutDefaults(), WithLedger(l), WithLogger(quietLogger())); err == nil {
		t.Fatal("New succeeded")
	}
	if _, err := l.Append(context.Background(), MustEvent(NewID(), EvRunQueued, RunQueuedData{Agent: "default", Input: "retained"})); err != nil {
		t.Fatalf("failed constructor closed the caller ledger: %v", err)
	}
}

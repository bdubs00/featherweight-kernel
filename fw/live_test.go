package fw

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// pieceModel answers each call with the text pieces of its script, after
// ready closes, and with a pause between the groups of pieces.
type pieceModel struct {
	ready  chan struct{}
	mu     sync.Mutex
	script [][]Delta
	resps  []*Response
}

func (p *pieceModel) Model() Model {
	return ModelFunc{StreamFn: func(ctx context.Context, _ *Request, on func(Delta)) (*Response, error) {
		select {
		case <-p.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		p.mu.Lock()
		groups, resp := p.script[0], p.resps[0]
		p.script, p.resps = p.script[1:], p.resps[1:]
		p.mu.Unlock()
		for i, d := range groups {
			if d.Type == "pause" {
				time.Sleep(3 * LiveCoalesce)
				continue
			}
			on(groups[i])
		}
		return resp, nil
	}}
}

// collectLive reads a live stream until it closes.
func collectLive(t *testing.T, ch <-chan LiveEvent) []LiveEvent {
	t.Helper()
	var out []LiveEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-deadline:
			t.Fatalf("the live stream did not end: %+v", out)
		}
	}
}

// The live stream of a run carries the text of the answer as the adapter
// yields it, coalesced, a tool event, a done event per call, and an idle
// event when the run finishes; the thinking of the provider never shows,
// and nothing enters the ledger.
func TestLiveStreamOfARun(t *testing.T) {
	ctx := testCtx(t)
	think := Block{Type: BlockOpaque, Provider: "fake", Raw: []byte(`{"thinking":"secret plan"}`)}
	tool := Block{Type: BlockToolUse, ID: "c1", Name: "echo", Input: []byte(`{"text":"x"}`)}
	p := &pieceModel{ready: make(chan struct{}),
		script: [][]Delta{
			{{Type: "block", Block: &think}, {Type: "text", Text: "Let me"}, {Type: "text", Text: " look."}, {Type: "block", Block: &tool}},
			{{Type: "text", Text: "Hel"}, {Type: "text", Text: "lo"}, {Type: "pause"}, {Type: "text", Text: " world"}},
		},
		resps: []*Response{callTools(use("c1", "echo", `{"text":"x"}`)), endTurn("Hello world")},
	}
	a := newTestAgent(t, p.Model(), WithTools(echoTool(nil)))
	r, err := a.Start(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	live, err := a.Live(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(p.ready)
	evs := collectLive(t, live)
	var types []string
	text := map[int]string{}
	var deltas2 int
	for _, e := range evs {
		types = append(types, e.Type)
		if e.Type == LiveDelta {
			text[e.Turn] += e.Text
			if e.Turn == 2 {
				deltas2++
			}
		}
		if strings.Contains(e.Text, "secret") {
			t.Fatalf("the thinking reached the stream: %+v", e)
		}
	}
	// The first piece goes out at once; the pieces within LiveCoalesce
	// after it wait for the next event: the tool event in turn 1, and the
	// timer in turn 2, which joins "Hel" and "lo".
	want := "delta delta tool done delta delta done idle"
	if got := strings.Join(types, " "); got != want {
		t.Fatalf("events = %s (%+v), want %s", got, evs, want)
	}
	if text[1] != "Let me look." || text[2] != "Hello world" || deltas2 != 2 {
		t.Fatalf("text = %v, %d deltas in turn 2", text, deltas2)
	}
	if evs[2] != (LiveEvent{Type: LiveTool, Turn: 1, Name: "echo", CallID: "c1"}) || evs[3] != (LiveEvent{Type: LiveDone, Turn: 1, StopReason: string(StopToolUse)}) {
		t.Fatalf("tool and done = %+v, %+v", evs[2], evs[3])
	}
	if evs[4].Text != "Hello" || evs[5].Text != " world" {
		t.Fatalf("deltas of turn 2 = %q, %q", evs[4].Text, evs[5].Text)
	}
	if last := evs[len(evs)-1]; last.State != StateCompleted {
		t.Fatalf("idle = %+v", last)
	}
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if strings.Contains(string(e.Type), "live") {
			t.Fatalf("the ledger holds %s", e.Type)
		}
	}
	// A run that finished gives one idle event, and the stream ends.
	again, err := a.Live(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evs := collectLive(t, again); len(evs) != 1 || evs[0].Type != LiveIdle {
		t.Fatalf("stream of a finished run = %+v", evs)
	}
}

// A client that cannot keep up is dropped, and the others go on.
func TestLiveDropsASlowClient(t *testing.T) {
	ctx := testCtx(t)
	a := newTestAgent(t, blockingModel(nil))
	r, err := a.Start(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	slow, err := a.Live(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := a.Live(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan int)
	go func() {
		n := 0
		for range fast {
			n++
			if n == 2*liveBuffer {
				got <- n
			}
		}
	}()
	for i := 0; i < 2*liveBuffer; i++ {
		a.live.event(r.ID, LiveEvent{Type: LiveTool, Turn: 1, Name: "echo"})
		time.Sleep(100 * time.Microsecond)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the fast client fell behind")
	}
	n := 0
	for range slow {
		n++
	}
	if n != liveBuffer {
		t.Fatalf("the slow client read %d events before the stream dropped it, want %d", n, liveBuffer)
	}
	if err := a.Cancel(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
}

// Only a viewer of the run reads its live stream, and a run takes at most
// MaxLiveClients clients.
func TestLiveAuthorizationAndBound(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"alice"}
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	mallory := WithPrincipal(ctx, &Principal{Subject: "mallory", Kind: KindUser})
	a := newTestAgent(t, blockingModel(nil), WithConfig(cfg))
	r, err := a.Start(alice, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Live(mallory, r.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a stranger read the live stream: %v", err)
	}
	if _, err := a.Live(alice, "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown run = %v", err)
	}
	cctx, cancel := context.WithCancel(alice)
	for i := 0; i < MaxLiveClients; i++ {
		if _, err := a.Live(cctx, r.ID); err != nil {
			t.Fatalf("client %d: %v", i+1, err)
		}
	}
	if _, err := a.Live(alice, r.ID); !errors.Is(err, ErrLiveFull) {
		t.Fatalf("client %d = %v, want ErrLiveFull", MaxLiveClients+1, err)
	}
	// The clients that go free their places.
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ch, err := a.Live(alice, r.ID)
		if err == nil {
			_ = ch
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the places did not come free: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.Cancel(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
}

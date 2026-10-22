package fw

// This file is the live stream of a run (docs/ARCHITECTURE.md sections 5.1
// and 8.2): the text of the answer as the model produces it, for a person
// who watches one run. The turn loop feeds the pieces that the adapter
// yields into a small fan-out per run. Nothing of it enters the ledger,
// and a client gets the pieces from the moment it connects, with no
// replay.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Types of a LiveEvent.
const (
	LiveDelta = "delta" // a piece of the text of the answer
	LiveTool  = "tool"  // the response holds a tool call
	LiveDone  = "done"  // the model call ended
	LiveIdle  = "idle"  // the run left the running state
)

// Bounds of the live stream.
const (
	// MaxLiveClients is the most clients of the live stream of one run.
	MaxLiveClients = 16
	// LiveCoalesce is the shortest time between two delta events: the
	// pieces of the text within it go out as one event.
	LiveCoalesce = 40 * time.Millisecond
	// liveBuffer is the number of events that a client can fall behind
	// before the stream drops it.
	liveBuffer = 64
)

// ErrLiveFull is the error of Agent.Live for a run that has
// MaxLiveClients clients.
var ErrLiveFull = errors.New("fw: the live stream of the run has as many clients as it takes")

// LiveEvent is one event of the live stream of a run. Type is LiveDelta,
// LiveTool, LiveDone, or LiveIdle. Turn is the turn of the model call.
// Text is the piece of the answer of a delta: the text that the adapter
// yielded, coalesced; the thinking of a provider is never part of it.
// Name and CallID name the tool call of a tool event. StopReason is the
// stop reason of a done event, "error" for a call that failed. State is
// the state of the run for an idle event.
type LiveEvent struct {
	Type       string   `json:"-"`
	Turn       int      `json:"turn,omitempty"`
	Text       string   `json:"text,omitempty"`
	Name       string   `json:"name,omitempty"`
	CallID     string   `json:"callId,omitempty"`
	StopReason string   `json:"stopReason,omitempty"`
	State      RunState `json:"state,omitempty"`
}

// liveHub is the fan-out of the live streams of an agent. active counts
// the runs with a client, so the turn loop pays one atomic load for a run
// that nobody watches.
type liveHub struct {
	active atomic.Int32
	mu     sync.Mutex
	runs   map[string]*liveRun
}

// liveRun is the fan-out of one run: its clients and the text that waits
// for the next delta event.
type liveRun struct {
	clients map[*liveClient]struct{}
	pending string
	turn    int
	last    time.Time
	timer   *time.Timer
}

// liveClient is one client. ch carries its events and closes when the
// client goes or falls behind; gone closes at the same time.
type liveClient struct {
	ch   chan LiveEvent
	gone chan struct{}
}

func (h *liveHub) subscribe(id string) (*liveClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runs == nil {
		h.runs = map[string]*liveRun{}
	}
	lr := h.runs[id]
	if lr == nil {
		lr = &liveRun{clients: map[*liveClient]struct{}{}}
		h.runs[id] = lr
		h.active.Add(1)
	}
	if len(lr.clients) >= MaxLiveClients {
		return nil, ErrLiveFull
	}
	c := &liveClient{ch: make(chan LiveEvent, liveBuffer), gone: make(chan struct{})}
	lr.clients[c] = struct{}{}
	return c, nil
}

// unsubscribe removes a client and closes its channel, once.
func (h *liveHub) unsubscribe(id string, c *liveClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[id]
	if lr == nil {
		return
	}
	h.dropLocked(id, lr, c)
}

func (h *liveHub) dropLocked(id string, lr *liveRun, c *liveClient) {
	if _, ok := lr.clients[c]; !ok {
		return
	}
	delete(lr.clients, c)
	close(c.ch)
	close(c.gone)
	if len(lr.clients) == 0 {
		if lr.timer != nil {
			lr.timer.Stop()
		}
		delete(h.runs, id)
		h.active.Add(-1)
	}
}

// sendLocked gives an event to every client of a run. A client whose
// buffer is full cannot keep up and is dropped: its stream ends, and it
// can connect again.
func (h *liveHub) sendLocked(id string, lr *liveRun, ev LiveEvent) {
	for c := range lr.clients {
		select {
		case c.ch <- ev:
		default:
			h.dropLocked(id, lr, c)
		}
	}
}

// flushLocked sends the text that waits as one delta event.
func (h *liveHub) flushLocked(id string, lr *liveRun) {
	if lr.pending == "" {
		return
	}
	ev := LiveEvent{Type: LiveDelta, Turn: lr.turn, Text: lr.pending}
	lr.pending, lr.last = "", time.Now()
	h.sendLocked(id, lr, ev)
}

// text adds a piece of the answer. It goes out at once when the last
// delta event is LiveCoalesce old, else with the next one.
func (h *liveHub) text(id string, turn int, piece string) {
	if h.active.Load() == 0 || piece == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[id]
	if lr == nil {
		return
	}
	if lr.turn != turn {
		h.flushLocked(id, lr)
		lr.turn = turn
	}
	lr.pending += piece
	wait := LiveCoalesce - time.Since(lr.last)
	if wait <= 0 {
		h.flushLocked(id, lr)
		return
	}
	if lr.timer == nil {
		lr.timer = time.AfterFunc(wait, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			lr.timer = nil
			if h.runs[id] == lr {
				h.flushLocked(id, lr)
			}
		})
	}
}

// event sends an event after the text that waits.
func (h *liveHub) event(id string, ev LiveEvent) {
	if h.active.Load() == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[id]
	if lr == nil {
		return
	}
	h.flushLocked(id, lr)
	h.sendLocked(id, lr, ev)
}

// sendTo sends an event to one client.
func (h *liveHub) sendTo(id string, c *liveClient, ev LiveEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	lr := h.runs[id]
	if lr == nil {
		return
	}
	if _, ok := lr.clients[c]; !ok {
		return
	}
	h.flushLocked(id, lr)
	select {
	case c.ch <- ev:
	default:
		h.dropLocked(id, lr, c)
	}
}

// liveOn returns the delta callback of a model call of a run in a turn:
// each text piece of the answer goes to the live stream, and a tool_use
// block that the adapter yields becomes a tool event. Thinking and other
// provider blocks never do.
func (a *Agent) liveOn(id string, turn int) func(Delta) {
	return func(d Delta) {
		if a.live.active.Load() == 0 {
			return
		}
		switch {
		case d.Type == "text":
			a.live.text(id, turn, d.Text)
		case d.Type == "block" && d.Block != nil && d.Block.Type == BlockToolUse:
			a.live.event(id, LiveEvent{Type: LiveTool, Turn: turn, Name: d.Block.Name, CallID: d.Block.ID})
		}
	}
}

// liveDone ends a model call on the live stream.
func (a *Agent) liveDone(id string, turn int, resp *Response, err error) {
	if a.live.active.Load() == 0 {
		return
	}
	reason := "error"
	if err == nil && resp != nil {
		reason = string(resp.StopReason)
	}
	a.live.event(id, LiveEvent{Type: LiveDone, Turn: turn, StopReason: reason})
}

// Live returns the live stream of a run: for the model call in flight and
// every later one while ctx lasts, a LiveDelta event for each piece of the
// text of the answer, at most one per LiveCoalesce, a LiveTool event when
// the response holds a tool call, a LiveDone event when the call ends, and
// a LiveIdle event when the run leaves the running state. A client that
// connects in the middle of a call gets the pieces from then on. Nothing of
// it enters the ledger. The channel closes when the run finishes, when ctx
// ends, or when the client falls behind by 64 events. With a principal in
// ctx, the caller needs the viewer role on the run. A run takes at most
// MaxLiveClients clients; one more gets ErrLiveFull.
func (a *Agent) Live(ctx context.Context, id string) (<-chan LiveEvent, error) {
	rs, err := a.k.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := a.authorizeRun(ctx, rs, ActionRunView); err != nil {
		return nil, err
	}
	c, err := a.live.subscribe(id)
	if err != nil {
		return nil, err
	}
	go func() {
		defer a.live.unsubscribe(id, c)
		working := true
		for {
			rs.mu.Lock()
			st, changed := rs.run.State, rs.changed
			rs.mu.Unlock()
			now := st == StateQueued || st == StateRunning
			if working && !now {
				a.live.sendTo(id, c, LiveEvent{Type: LiveIdle, State: st})
			}
			if st.Terminal() {
				return
			}
			working = now
			select {
			case <-ctx.Done():
				return
			case <-c.gone:
				return
			case <-changed:
			}
		}
	}()
	return c.ch, nil
}

package a2a

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	sdk "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// pusher keeps push notification configs in memory, per task, and POSTs the
// task to every config of a task when its A2A state changes. The configs do
// not survive a restart. Each task has at most one delivery goroutine, so a
// receiver sees the states of one task in order.
type pusher struct {
	a      *fw.Agent
	log    *slog.Logger
	store  *push.InMemoryPushConfigStore
	sender *push.HTTPPushSender
	once   sync.Once

	mu    sync.Mutex
	last  map[string]sdk.TaskState // last state queued per task
	queue map[string][]*sdk.Task   // tasks waiting for delivery
	busy  map[string]bool          // a delivery goroutine runs for the task
}

// save stores a config for a task. The first config registers the observer,
// so an agent without push configs pays nothing per event.
func (p *pusher) save(ctx context.Context, taskID string, cfg *sdk.PushConfig) (*sdk.PushConfig, error) {
	c := *cfg
	c.TaskID = sdk.TaskID(taskID)
	saved, err := p.store.Save(ctx, c.TaskID, &c)
	if err != nil {
		return nil, err
	}
	p.once.Do(func() { p.a.Observe(fw.ObserverFunc(p.observe)) })
	return saved, nil
}

// observe runs inside the kernel append, so it only reads and queues.
func (p *pusher) observe(e fw.Event) {
	switch e.Type {
	case fw.EvRunQueued, fw.EvRunStarted, fw.EvRunResumed, fw.EvMessageReceived,
		fw.EvApprovalRequested, fw.EvApprovalDecided, fw.EvRunSuspended, fw.EvRunFinished:
	default:
		return
	}
	cfgs, err := p.store.List(context.Background(), sdk.TaskID(e.Subject))
	if err != nil || len(cfgs) == 0 {
		return
	}
	// The kernel holds the append lock of the run, so the status here is
	// the status right after e.
	r, err := p.a.Get(context.Background(), e.Subject)
	if err != nil {
		return
	}
	st := TaskState(r.State)
	p.mu.Lock()
	if p.last[r.ID] == st {
		p.mu.Unlock()
		return
	}
	p.last[r.ID] = st
	p.queue[r.ID] = append(p.queue[r.ID], toTask(r, nil))
	start := !p.busy[r.ID]
	p.busy[r.ID] = true
	p.mu.Unlock()
	if start {
		go p.deliver(r.ID)
	}
}

func (p *pusher) deliver(id string) {
	for {
		p.mu.Lock()
		q := p.queue[id]
		if len(q) == 0 {
			delete(p.queue, id)
			p.busy[id] = false
			p.mu.Unlock()
			return
		}
		t := q[0]
		p.queue[id] = q[1:]
		p.mu.Unlock()
		cfgs, err := p.store.List(context.Background(), sdk.TaskID(id))
		if err != nil {
			continue
		}
		for _, c := range cfgs {
			ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
			err := p.sender.SendPush(ctx, c, t)
			cancel()
			if err != nil {
				// The URL can hold a token in its query, so the log names
				// the host only.
				msg := strings.ReplaceAll(err.Error(), c.URL, hostOf(c.URL))
				p.log.Warn("a2a push failed", "task", id, "config", c.ID, "state", string(t.Status.State), "error", msg)
			}
		}
	}
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid url)"
	}
	return u.Scheme + "://" + u.Host
}

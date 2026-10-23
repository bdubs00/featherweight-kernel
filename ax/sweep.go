package ax

// This file is the sweep of the child Tasks (docs/ARCHITECTURE.md section
// 11.9). ax keeps a Task until a caller deletes it: a Task has no owner, no
// label, and no time to live, and ax collects nothing. So the process that
// creates child Tasks also removes them. A service lists the Tasks of the
// atespace soon after the start of the process and then every sweepEvery,
// and takes each Task whose FW_PARENT_MEMBER names this Task: a child of
// this process, or a child of an earlier process of the same Task whose
// ledger is gone. A child Task goes, with the child Tasks under it, once
// none of them holds an open run and their runs ended ax.childRetention
// ago. The clocks are the creation and the suspend of each Task on the ax
// API and the newest update of a run in the ledger of the child, so a
// restart of this process loses none of them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func init() {
	fw.RegisterService(startSweep)
}

// The intervals of the passes; tests shorten them.
var (
	sweepFirst = 10 * time.Second // the first pass after the start of the process
	sweepEvery = 15 * time.Minute // the passes after it
)

// Limits of the sweep.
const (
	sweepWakes    = 8                // the sleeping child Tasks that one pass wakes for a read
	sweepPage     = 100              // the Tasks of one ListTasks call
	sweepMaxTasks = 10_000           // the Tasks that one pass reads at most
	sweepReady    = 3 * time.Minute  // how long a woken child Task gets to run
	statusTimeout = 2 * time.Minute  // how long the status read of a child that boots gets
	callTimeout   = 30 * time.Second // how long one call of the ax API gets

	condReady          = "Ready"
	condWorkspaceReady = "WorkspaceReady"
	reasonSuspended    = "TaskSuspended"
)

// sweeper removes the child Tasks of this Task that nothing needs.
type sweeper struct {
	s        *Spawner
	atespace string
	member   string        // this Task, "ax:<atespace>/<task>"
	hold     func() func() // fw.Agent.Hold: the process is not idle while a pass wakes a child
	done     chan struct{}
}

// startSweep starts the sweep beside an agent whose spawner is the ax
// driver, in a process that serves in a Task and can start children. A
// child that cannot start children has none, and no ax API host in its
// egress either.
func startSweep(_ context.Context, a *fw.Agent) (io.Closer, error) {
	s, ok := a.Spawner().(*Spawner)
	if !ok || s.retention < 0 || s.cfg.Spawn.Disabled {
		return nil, nil
	}
	task, _ := Stashed()
	atespace, name := Identity(task)
	if atespace == "" || name == "" {
		return nil, nil
	}
	w := &sweeper{s: s, atespace: atespace, member: "ax:" + atespace + "/" + name, hold: a.Hold, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	go w.loop(ctx)
	return closer(func() error {
		cancel()
		<-w.done
		return nil
	}), nil
}

type closer func() error

func (c closer) Close() error { return c() }

// loop runs a pass soon after the start and then every sweepEvery, until
// ctx ends.
func (w *sweeper) loop(ctx context.Context) {
	defer close(w.done)
	t := time.NewTimer(sweepFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		w.pass(ctx)
		t.Reset(sweepEvery)
	}
}

// passState is what one pass counts: the sleeping child Tasks that it
// woke, and the release of the hold that keeps this process awake from the
// first wake to the end of the pass, so a suspend of this Task does not cut
// a child that the pass woke before its delete.
type passState struct {
	wakes   int
	release func()
}

// pass removes each child Task that is due, with the Tasks under it. It
// reads the Tasks of a tree from the top, stops at the first one that must
// stay, and deletes the tree from the bottom, so a Task never loses its
// parent while it stays.
func (w *sweeper) pass(ctx context.Context) {
	client, err := w.s.ax()
	if err != nil {
		w.s.log.Warn("sweep the child Tasks", "error", err)
		return
	}
	tasks, err := w.list(ctx, client)
	if err != nil {
		w.s.log.Warn("list the Tasks to sweep the child Tasks", "atespace", w.atespace, "error", err)
		return
	}
	below := map[string][]*v1alpha1.Task{}
	for _, t := range tasks {
		if p := envOf(t, EnvParentMember); p != "" {
			below[p] = append(below[p], t)
		}
	}
	var ps passState
	defer func() {
		if ps.release != nil {
			ps.release()
		}
	}()
	for _, child := range below[w.member] {
		tree := subtree(child, below, w.atespace)
		due := true
		for _, t := range tree {
			if ctx.Err() != nil {
				return
			}
			ok, why := w.due(ctx, client, t, &ps)
			if !ok {
				w.s.log.Debug("a child Task stays", "atespace", w.atespace, "task", t.GetMetadata().GetName(), "reason", why)
				due = false
				break
			}
		}
		if !due {
			continue
		}
		for i := len(tree) - 1; i >= 0; i-- {
			name := tree[i].GetMetadata().GetName()
			cctx, cancel := context.WithTimeout(ctx, callTimeout)
			_, err := client.DeleteTask(cctx, &v1alpha1.DeleteTaskRequest{Atespace: w.atespace, Name: name})
			cancel()
			if err != nil && status.Code(err) != codes.NotFound {
				// The Task above stays, and the next pass tries again.
				w.s.log.Warn("delete a child Task whose runs ended", "atespace", w.atespace, "task", name, "error", err)
				break
			}
			w.s.log.Info("child Task removed, its runs ended", "atespace", w.atespace, "task", name, "retention", w.s.retention)
		}
	}
}

// list reads the Tasks of the atespace, page by page.
func (w *sweeper) list(ctx context.Context, client v1alpha1.AXClient) ([]*v1alpha1.Task, error) {
	var out []*v1alpha1.Task
	for offset := int64(0); offset < sweepMaxTasks; offset += sweepPage {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		resp, err := client.ListTasks(cctx, &v1alpha1.ListTasksRequest{Atespace: w.atespace, Limit: sweepPage, Offset: offset})
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTasks()...)
		if len(resp.GetTasks()) < sweepPage {
			break
		}
	}
	return out, nil
}

// subtree returns a child Task and the Tasks under it, each after its
// parent.
func subtree(top *v1alpha1.Task, below map[string][]*v1alpha1.Task, atespace string) []*v1alpha1.Task {
	out := []*v1alpha1.Task{top}
	seen := map[string]bool{top.GetMetadata().GetName(): true}
	for i := 0; i < len(out); i++ {
		for _, t := range below["ax:"+atespace+"/"+out[i].GetMetadata().GetName()] {
			if name := t.GetMetadata().GetName(); !seen[name] {
				seen[name] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// due reports whether nothing needs the child Task t any more, and why not.
// A Task that ax deletes already is due, and so is a Task that failed
// before it ran. A Task younger than the retention stays unread. A Task
// that runs answers its status, which tells whether its runs ended long
// enough ago. A Task that sleeps is read only once it slept for the
// retention, because the read wakes it: the pass resumes it through the ax
// API, as Find does, so the phase follows, reads it, and puts it back to
// sleep when it stays.
func (w *sweeper) due(ctx context.Context, client v1alpha1.AXClient, t *v1alpha1.Task, ps *passState) (bool, string) {
	s, name, phase := w.s, t.GetMetadata().GetName(), t.GetStatus().GetPhase()
	now := s.now()
	if phase == v1alpha1.PhaseTerminating {
		return true, ""
	}
	if c := t.GetMetadata().GetCreateTime(); c == nil || now.Sub(c.AsTime()) < s.retention {
		return false, "it is younger than the retention"
	}
	switch phase {
	case phaseFailed:
		if condition(t, condWorkspaceReady) != nil {
			return false, "it failed after it ran, so it can hold runs that nobody can read; a person decides"
		}
		return true, ""
	case phaseRunning:
		ok, why, _ := w.ended(ctx, name, now)
		return ok, why
	case phaseSuspended:
	default:
		return false, fmt.Sprintf("its phase is %q", phase)
	}
	if c := condition(t, condReady); c != nil && c.GetReason() == reasonSuspended && now.Sub(c.GetUpdateTime().AsTime()) < s.retention {
		return false, "it slept for less than the retention"
	}
	if ps.wakes >= sweepWakes {
		return false, "this pass woke enough child Tasks"
	}
	ps.wakes++
	if ps.release == nil {
		ps.release = w.hold()
	}
	rctx, cancel := context.WithTimeout(ctx, sweepReady)
	err := s.awaitRunning(rctx, client, w.atespace, name)
	cancel()
	if err != nil {
		return false, err.Error()
	}
	ok, why, works := w.ended(ctx, name, now)
	if !ok && !works && ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		_, err := client.SuspendTask(cctx, &v1alpha1.SuspendTaskRequest{Atespace: w.atespace, Name: name})
		cancel()
		if err != nil {
			s.log.Warn("suspend a child Task that the sweep woke", "atespace", w.atespace, "task", name, "error", err)
		}
		s.log.Info("a child Task stays", "atespace", w.atespace, "task", name, "reason", why)
	}
	return ok, why
}

// childStatus is the part of GET /v1/status of a child that the sweep
// reads (docs/ARCHITECTURE.md section 11.8).
type childStatus struct {
	Runs struct {
		Active int `json:"active"`
		Total  int `json:"total"`
	} `json:"runs"`
	Idle      bool       `json:"idle"`
	IdleSince *time.Time `json:"idleSince"`
	Blocked   bool       `json:"blocked"`
}

// ended reads the status of the child Task name and reports whether its
// runs ended the retention ago, why not, and whether a run works there. A
// Task without runs never got its run, and it is older than the retention.
// The status counts the runs that the static token can view, so that token
// must see every run of a child, as it sees every run of a pool.
func (w *sweeper) ended(ctx context.Context, name string, now time.Time) (bool, string, bool) {
	st, err := w.status(ctx, w.atespace+"/"+name)
	switch {
	case err != nil:
		return false, err.Error(), false
	case st.Blocked:
		return false, "a storage failure blocks it; a person decides", false
	case st.Runs.Active > 0:
		return false, fmt.Sprintf("it holds %d open runs", st.Runs.Active), !st.Idle
	case !st.Idle:
		return false, "it works", true
	case st.Runs.Total == 0:
		return true, "", false
	case st.IdleSince == nil || now.Sub(*st.IdleSince) < w.s.retention:
		return false, "its runs ended less than the retention ago", false
	}
	return true, "", false
}

// status reads GET /v1/status of a child through the router. A child that
// boots answers a gateway error for a while, so the read is sent again.
func (w *sweeper) status(ctx context.Context, actor string) (*childStatus, error) {
	deadline := time.Now().Add(statusTimeout)
	wait := time.Second
	for {
		code, answer, err := w.s.do(ctx, http.MethodGet, actor, "/v1/status", nil, "")
		var noToken *noTokenError
		switch {
		case errors.As(err, &noToken):
			return nil, err
		case err == nil && code == http.StatusOK:
			var st childStatus
			if err := json.Unmarshal(answer, &st); err != nil {
				return nil, fmt.Errorf("ax: the status of the child %s: %w", actor, err)
			}
			return &st, nil
		case err == nil && code != http.StatusBadGateway && code != http.StatusServiceUnavailable && code != http.StatusGatewayTimeout:
			return nil, problemOf(actor, code, answer)
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case time.Now().After(deadline):
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("ax: the child %s did not answer its status within %s: %s", actor, statusTimeout, http.StatusText(code))
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		wait = min(2*wait, 10*time.Second)
	}
}

// envOf returns the value of a variable of a Task, or "".
func envOf(t *v1alpha1.Task, name string) string {
	for _, e := range t.GetSpec().GetEnv() {
		if e.GetName() == name {
			return e.GetValue()
		}
	}
	return ""
}

// condition returns the condition of a Task of the type, or nil.
func condition(t *v1alpha1.Task, typ string) *v1alpha1.Condition {
	for _, c := range t.GetStatus().GetConditions() {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

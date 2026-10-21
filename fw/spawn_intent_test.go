package fw

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var _ Planner = (*crashSpawner)(nil)
var _ Planner = (*otherSpawner)(nil)

// syncProbe checks, inside Spawn, that the record of the child is in the
// ledger file, and fsynced, before the spawner creates anything.
type syncProbe struct {
	*otherSpawner
	t      *testing.T
	l      *FileLedger
	before int64
	probed atomic.Bool
}

func (p *syncProbe) Spawn(ctx context.Context, parent *Run, req RunRequest) (*Run, string, error) {
	p.probed.Store(true)
	if n := p.l.syncs.Load(); n != p.before+1 {
		p.t.Errorf("Spawn ran after %d fsync calls, want %d (one for the record of the child)", n, p.before+1)
	}
	found := false
	for e, err := range p.l.Read(ctx, parent.ID, 0) {
		if err != nil {
			p.t.Errorf("read the ledger: %v", err)
			break
		}
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil && d.Child == req.ID && d.Member == "other" && !d.Started {
			found = true
		}
	}
	if !found {
		p.t.Error("the record of the child was not in the ledger file when Spawn ran")
	}
	return p.otherSpawner.Spawn(ctx, parent, req)
}

// The record of a child is on disk before the spawner creates its process,
// and names the id of the child run and its member, which Plan gave.
func TestSpawnChildRecordIsDurableBeforeTheSpawner(t *testing.T) {
	ctx := testCtx(t)
	l, err := OpenFileLedger(filepath.Join(t.TempDir(), "runs"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	probe := &syncProbe{otherSpawner: &otherSpawner{other: other}, t: t, l: l}
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithSpawner(probe))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	probe.before = l.syncs.Load()
	child, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x", WithIdempotencyKey("k1")))
	if err != nil {
		t.Fatal(err)
	}
	if !probe.probed.Load() {
		t.Fatal("Spawn never ran")
	}
	if _, ok := IDTime(child.ID); !ok {
		t.Fatalf("the child id %q is not an id of NewID", child.ID)
	}
	var recs []RunChildData
	for _, e := range ledgerEvents(t, l, parent.ID) {
		var d RunChildData
		if e.Type == EvRunChild && e.Decode(&d) == nil {
			recs = append(recs, d)
		}
	}
	if len(recs) != 2 || recs[0].Child != child.ID || recs[0].Started || !strings.HasPrefix(recs[0].Key, "k1#") || recs[0].Input != "do x" ||
		recs[0].Metadata[MetaParent] != parent.ID || !recs[1].Started || recs[1].Member != "other" {
		t.Fatalf("records of the child = %+v", recs)
	}
}

// refuseStarts is a ledger that refuses the record of a start, as a disk
// that is full refuses it.
type refuseStarts struct {
	Ledger
	refused atomic.Int32
	after   bool
}

func (l *refuseStarts) Append(ctx context.Context, e Event) (uint64, error) {
	var d RunChildData
	if e.Type == EvRunChild && json.Unmarshal(e.Data, &d) == nil && d.Started {
		l.refused.Add(1)
		if l.after {
			if _, err := l.Ledger.Append(ctx, e); err != nil {
				return 0, err
			}
		}
		return 0, errors.New("disk full")
	}
	return l.Ledger.Append(ctx, e)
}

// A failed start append can be on disk already. The parent fail-stops and
// retains the child and its key without retrying or inventing a terminal
// record. Restart reconstructs the start or finds the child from its intent.
func TestSpawnChildWhoseStartIsNotRecorded(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "not stored", true: "ambiguous fsync"}[after], func(t *testing.T) {
			ctx := testCtx(t)
			other := newTestAgent(t, blockingModel(nil))
			sp := newCrashSpawner(other)
			base := NewMemLedger()
			l := &refuseStarts{Ledger: base, after: after}
			a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithSpawner(sp))
			parent, err := a.Run(ctx, "parent work")
			mustState(t, parent, err, StateCompleted)
			req := NewRunRequest("do x", WithIdempotencyKey("same"))
			_, err = a.SpawnChild(ctx, parent.ID, req)
			if !errors.Is(err, ErrStorageUnavailable) || !strings.Contains(err.Error(), "record the start") {
				t.Fatalf("SpawnChild = %v", err)
			}
			if n := l.refused.Load(); n != 1 {
				t.Fatalf("the failed append was retried %d times", n)
			}
			if a.Ready() {
				t.Fatal("storage failure left the parent ready")
			}
			list, _ := other.List(ctx)
			if len(list) != 1 {
				t.Fatalf("the child process holds %+v", list)
			}
			child := list[0]
			if _, created, abandoned := sp.counts(); len(created) != 1 || len(abandoned) != 0 {
				t.Fatalf("created %v, abandoned %v", created, abandoned)
			}
			r, err := a.Get(ctx, child.ID)
			if err != nil || r.ID != child.ID || r.Blocked == "" || r.State.Terminal() {
				t.Fatalf("blocked child = %+v, %v", r, err)
			}
			if _, ok := a.remoteChild(child.ID); !ok {
				t.Fatal("ambiguous child was forgotten")
			}
			if _, err := a.SpawnChild(ctx, parent.ID, req); !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("retry during storage failure = %v", err)
			}
			a.Close(ctx)
			resumed := newTestAgent(t, newScript().Model(), WithLedger(base), WithSpawner(sp))
			r, err = resumed.SpawnChild(ctx, parent.ID, req)
			if err != nil || r.ID != child.ID {
				t.Fatalf("retry after restart = %+v, %v", r, err)
			}
			if _, created, abandoned := sp.counts(); len(created) != 1 || len(abandoned) != 0 {
				t.Fatalf("restart duplicated or abandoned the child: created %v, abandoned %v", created, abandoned)
			}
			if err := other.Cancel(ctx, child.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// failingSpawner refuses every child, after its record.
type failingSpawner struct{ *otherSpawner }

func (failingSpawner) Spawn(context.Context, *Run, RunRequest) (*Run, string, error) {
	return nil, "", errors.New("no room for a Task")
}

// A start that the spawner refuses is recorded as a failed child with the
// reason, so a restart does not look for it. The parent lists it, and it
// does not count toward spawn.maxChildren.
func TestSpawnChildThatTheSpawnerRefuses(t *testing.T) {
	ctx := testCtx(t)
	l := NewMemLedger()
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	cfg := DefaultConfig()
	cfg.Spawn.MaxChildren = 1
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithLedger(l), WithConfig(cfg), WithSpawner(failingSpawner{&otherSpawner{other: other}}))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	if _, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x")); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Fatalf("SpawnChild = %v", err)
	}
	var d RunChildData
	firstOf(t, ledgerEvents(t, l, parent.ID), EvRunChild, &d)
	r, err := a.Get(ctx, d.Child)
	if err != nil || r.State != StateFailed || !strings.Contains(r.Error, "did not start in other") || !strings.Contains(r.Error, "no room") {
		t.Fatalf("the refused child = %+v, %v", r, err)
	}
	listed := func(a *Agent) {
		t.Helper()
		kids, err := a.Children(ctx, parent.ID)
		if err != nil || len(kids) != 1 || kids[0].ID != d.Child || kids[0].State != StateFailed || kids[0].Parent != parent.ID {
			t.Fatalf("children of the parent = %+v, %v", kids, err)
		}
	}
	listed(a)
	// The next process does not look for the refused child, and the slot
	// of spawn.maxChildren is free here and there.
	b := newTestAgent(t, newScript().Model(), WithLedger(l), WithConfig(cfg), WithSpawner(&otherSpawner{other: other}))
	if b.Starting() != 0 {
		t.Fatalf("Starting = %d after the restart", b.Starting())
	}
	if r, err := b.Get(ctx, d.Child); err != nil || r.State != StateFailed {
		t.Fatalf("the refused child after the restart = %+v, %v", r, err)
	}
	listed(b)
	if _, err := b.SpawnChild(ctx, parent.ID, NewRunRequest("do y")); err != nil {
		t.Fatalf("a child after the refused one = %v", err)
	}
}

// Two starts of a child of one parent under the same key, one after the
// other or at the same time, give one child.
func TestSpawnChildUnderTheSameKeyGivesOneChild(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("one"), endTurn("two")).Model())
	sp := newCrashSpawner(other)
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)

	reached, release := sp.pauseAt(beforeRun)
	var wg sync.WaitGroup
	ids := make([]string, 3)
	errs := make([]error, 3)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x", WithIdempotencyKey("call-1")))
			errs[i] = err
			if r != nil {
				ids[i] = r.ID
			}
		}()
	}
	<-reached
	// While the start is open, the parent answers for the child.
	kids, err := a.Children(ctx, parent.ID)
	if err != nil || len(kids) != 1 || kids[0].State != StateQueued || !strings.HasPrefix(kids[0].Member, "task-") {
		t.Fatalf("children while the start is open = %+v, %v", kids, err)
	}
	if a.Starting() != 1 {
		t.Fatalf("Starting = %d while the child starts", a.Starting())
	}
	release()
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("start %d = %v", i, errs[i])
		}
	}
	if ids[0] == "" || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("the starts under one key gave %v", ids)
	}
	if spawns, _, _ := sp.counts(); spawns != 1 {
		t.Fatalf("Spawn ran %d times for one key", spawns)
	}
	again, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x", WithIdempotencyKey("call-1")))
	if err != nil || again.ID != ids[0] || again.Member != taskOf(ids[0]) || again.Parent != parent.ID {
		t.Fatalf("a later start under the key = %+v, %v", again, err)
	}
	if other, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x", WithIdempotencyKey("call-2"))); err != nil || other.ID == ids[0] {
		t.Fatalf("a start under another key = %+v, %v", other, err)
	}
}

// Cancel while a child starts ends the start: the spawner leaves nothing
// behind, and the child is recorded as canceled.
func TestSpawnChildCanceledWhileItStarts(t *testing.T) {
	ctx := testCtx(t)
	other := newTestAgent(t, newScript(endTurn("child done")).Model())
	sp := newCrashSpawner(other)
	a := newTestAgent(t, newScript(endTurn("parent done")).Model(), WithSpawner(sp))
	parent, err := a.Run(ctx, "parent work")
	mustState(t, parent, err, StateCompleted)
	reached, release := sp.stop(beforeRun)
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := a.SpawnChild(ctx, parent.ID, NewRunRequest("do x"))
		done <- err
	}()
	<-reached
	ids, _ := recordedChildren(t, a.Ledger(), parent.ID)
	if len(ids) != 1 {
		t.Fatalf("records = %v", ids)
	}
	if err := a.Decide(ctx, ids[0], ApprovalDecision{ApprovalID: "x", Decision: "allow"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Decide while the child starts = %v", err)
	}
	if err := a.Cancel(ctx, ids[0]); err != nil {
		t.Fatalf("Cancel while the child starts: %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("SpawnChild of a canceled start returned no error")
	}
	r, err := a.Get(ctx, ids[0])
	if err != nil || r.State != StateCanceled {
		t.Fatalf("the canceled child = %+v, %v", r, err)
	}
	if list, _ := other.List(ctx); len(list) != 0 {
		t.Fatalf("the child process holds %+v", list)
	}
	if _, created, _ := sp.counts(); len(created) != 1 || sp.alive(created[0]) == nil {
		t.Fatalf("the canceled start left its process: %v", created)
	}
}

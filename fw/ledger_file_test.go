package fw

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileLedgerAppendReadRunsIndex(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for i, typ := range []string{EvRunQueued, EvRunStarted, EvApprovalRequested, EvApprovalDecided} {
		seq, err := l.Append(ctx, MustEvent("A", typ, TurnData{Turn: i}))
		if err != nil || seq != uint64(i+1) {
			t.Fatalf("append %d = %d, %v", i, seq, err)
		}
	}
	if _, err := l.Append(ctx, MustEvent("B", EvRunQueued, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ctx, MustEvent("A", EvRunFinished, RunFinishedData{Outcome: OutcomeCompleted})); err != nil {
		t.Fatal(err)
	}
	if n := l.syncs.Load(); n != 5 {
		t.Fatalf("fsync calls = %d, want 5 (two queues, two approvals, one terminal event)", n)
	}
	if _, err := l.Append(ctx, MustEvent("../x", EvRunQueued, nil)); err == nil {
		t.Fatal("a path in the run id was accepted")
	}

	var got []Event
	for e, err := range l.Read(ctx, "A", 2) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if len(got) != 4 || got[0].Seq != 2 || got[3].Type != EvRunFinished || got[0].Source != "fw://local/A" {
		t.Fatalf("read = %v", eventTypes(got))
	}
	var ids []string
	for id, err := range l.Runs(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != 2 || ids[0] != "A" || ids[1] != "B" {
		t.Fatalf("runs = %v", ids)
	}
	f, err := os.Open(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	lines := 0
	for sc := bufio.NewScanner(f); sc.Scan(); lines++ {
		var ent indexEntry
		if err := json.Unmarshal(sc.Bytes(), &ent); err != nil {
			t.Fatal(err)
		}
	}
	if lines != 2 {
		t.Fatalf("index lines = %d, want one per run", lines)
	}
	for _, p := range []string{dir, filepath.Join(dir, "A")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode = %v, %v", p, st.Mode(), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "A", "ledger.jsonl")); err != nil {
		t.Fatal(err)
	}
}

func TestFileLedgerReopenAndTornLine(t *testing.T) {
	ctx := testCtx(t)
	dir := t.TempDir()
	l, _ := OpenFileLedger(dir)
	for range 2 {
		if _, err := l.Append(ctx, MustEvent("R", EvRunQueued, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// A crash in the middle of a write leaves a partial line.
	f, err := os.OpenFile(filepath.Join(dir, "R", "ledger.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"specversion":"1.0","id":"broken`)
	f.Close()

	l2, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	n := 0
	for _, err := range l2.Read(ctx, "R", 0) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("read %d events, want 2", n)
	}
	seq, err := l2.Append(ctx, MustEvent("R", EvRunStarted, nil))
	if err != nil || seq != 3 {
		t.Fatalf("append after reopen = %d, %v", seq, err)
	}
	n = 0
	for _, err := range l2.Read(ctx, "R", 0) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 3 {
		t.Fatalf("read %d events after repair, want 3", n)
	}
}

func TestFileLedgerWatch(t *testing.T) {
	ctx := testCtx(t)
	l, _ := OpenFileLedger(t.TempDir())
	defer l.Close()
	for range 3 {
		if _, err := l.Append(ctx, MustEvent("W", EvTurnStarted, nil)); err != nil {
			t.Fatal(err)
		}
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := l.Watch(wctx, "W", 2)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := l.Append(ctx, MustEvent("W", EvTurnFinished, nil)); err != nil {
			t.Fatal(err)
		}
	}
	var seqs []uint64
	timeout := time.After(5 * time.Second)
	for len(seqs) < 4 {
		select {
		case e := <-ch:
			seqs = append(seqs, e.Seq)
		case <-timeout:
			t.Fatalf("watch got %v", seqs)
		}
	}
	for i, s := range seqs {
		if s != uint64(i+2) {
			t.Fatalf("watch seqs = %v", seqs)
		}
	}
	cancel()
	for range ch {
	}
}

func TestFileLedgerDriverWithAgent(t *testing.T) {
	ctx := testCtx(t)
	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.Home = home
	l, err := OpenLedger(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fl, ok := l.(*FileLedger); !ok || fl.Dir() != filepath.Join(home, "runs") {
		t.Fatalf("ledger = %T", l)
	}
	if err := l.(*FileLedger).Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Ledger.Path = filepath.Join(home, "elsewhere")
	l, err = OpenLedger(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if l.(*FileLedger).Dir() != cfg.Ledger.Path {
		t.Fatal("Ledger.Path does not override the directory")
	}
	if err := l.(*FileLedger).Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Ledger.Path = ""

	// The agent opens the "file" driver from the configuration.
	t.Setenv("FW_ATESPACE", "")
	a, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript(endTurn("on disk")).Model()), WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Run(ctx, "persist me")
	mustState(t, r, err, StateCompleted)
	a.Close(ctx)

	b, err := New(WithConfig(cfg), WithoutDefaults(), WithAuthn(NoAuthn{}), WithModel(newScript().Model()), WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx)
	got, err := b.Get(ctx, r.ID)
	if err != nil || got.Output != "on disk" || got.State != StateCompleted || got.Usage.Turns != 1 {
		t.Fatalf("rebuilt run = %+v, %v", got, err)
	}
}

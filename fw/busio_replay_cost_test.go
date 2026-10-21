package fw

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lineOffsets returns the byte offset of each line of the ledger of a run.
func lineOffsets(t *testing.T, l *FileLedger, id string) []int64 {
	t.Helper()
	data, err := os.ReadFile(l.ledgerPath(id))
	if err != nil {
		t.Fatal(err)
	}
	var offs []int64
	var off int64
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			break
		}
		offs = append(offs, off)
		off += int64(len(line))
	}
	return offs
}

// A load checks the offset of a cursor before it trusts it. A cursor that
// does not hold goes, and the egress sends the run again from the start.
func TestFileLedgerEgressCursorHintsAreChecked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l, err := OpenFileLedger(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := appendReplayEvent(t, l, "run", EvRunStarted)
	second := appendReplayEvent(t, l, "run", EvTurnStarted)
	appendReplayEvent(t, l, "run", EvRunFinished)
	offs := lineOffsets(t, l, "run")
	path := l.egressCursorPath("route", "run")
	if err := l.SaveEgressCursor(ctx, "route", "run", second.Seq); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cursor := func(seq uint64, id string, off *int64) string {
		b, _ := json.Marshal(fileEgressCursor{Seq: seq, ID: id, Offset: off})
		return string(b)
	}
	at := func(v int64) *int64 { return &v }
	cases := []struct {
		name, data string
		want       uint64
	}{
		{"valid", cursor(second.Seq, second.ID, at(offs[1])), second.Seq},
		{"no offset", cursor(second.Seq, second.ID, nil), 0},
		{"negative offset", cursor(second.Seq, second.ID, at(-5)), 0},
		{"offset past the end", cursor(second.Seq, second.ID, at(1<<40)), 0},
		{"offset inside a line", cursor(second.Seq, second.ID, at(offs[1]+3)), 0},
		{"wrong id", cursor(second.Seq, "01NOTTHEEVENT0000000000000", at(offs[1])), 0},
		{"old sequence at a new offset", cursor(first.Seq, first.ID, at(offs[1])), 0},
		{"not JSON", "{", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			l, err := OpenFileLedger(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			seq, err := l.LoadEgressCursor(ctx, "route", "run")
			if err != nil || seq != tc.want {
				t.Fatalf("cursor = %d, %v, want %d", seq, err, tc.want)
			}
			if _, err := os.Stat(path); (err == nil) != (tc.want != 0) {
				t.Fatalf("cursor file present = %v, want %v", err == nil, tc.want != 0)
			}
		})
	}
}

// Once the egress caught up, a pass reads no line of the history and makes
// no fsync, however many runs the ledger holds.
func TestBusEgressIdlePassesTouchNoDisk(t *testing.T) {
	ctx := context.Background()
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		id := fmt.Sprintf("run%02d", i)
		for range 4 {
			appendReplayEvent(t, l, id, EvTurnStarted)
		}
		appendReplayEvent(t, l, id, EvRunFinished)
	}
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l, bus)
	never := func() bool { return false }
	for {
		more, err := e.pass(ctx, true, never)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}
	if n := len(bus.snapshot()); n != 200 {
		t.Fatalf("published %d events, want 200", n)
	}
	syncs, reads := l.esyncs.Load(), l.reads.Load()
	for i := range 6 {
		if _, err := e.pass(ctx, i%2 == 0, never); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(bus.snapshot()); n != 200 {
		t.Fatalf("idle passes published again: %d events", n)
	}
	if d := l.esyncs.Load() - syncs; d != 0 {
		t.Fatalf("idle passes made %d fsyncs", d)
	}
	if d := l.reads.Load() - reads; d != 0 {
		t.Fatalf("idle passes decoded %d lines", d)
	}
	// A new event costs the lines after the cursor, not the history.
	ev := appendReplayEvent(t, l, "run07", EvRunAnnotated)
	e.observe(ev)
	reads = l.reads.Load()
	if _, err := e.pass(ctx, false, never); err != nil {
		t.Fatal(err)
	}
	if got := bus.snapshot(); got[len(got)-1].ID != ev.ID {
		t.Fatal("the new event was not published")
	}
	if d := l.reads.Load() - reads; d > 2 {
		t.Fatalf("one new event decoded %d lines", d)
	}
}

// A Read after the last egress cursor starts at the line of the cursor,
// and a hint that does not hold makes it read from the first line.
func TestFileLedgerReadStartsAtTheEgressCursor(t *testing.T) {
	ctx := context.Background()
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 200 {
		appendReplayEvent(t, l, "run", EvTurnStarted)
	}
	if err := l.SaveEgressCursor(ctx, "route", "run", 190); err != nil {
		t.Fatal(err)
	}
	read := func() []uint64 {
		var seqs []uint64
		for ev, err := range l.Read(ctx, "run", 191) {
			if err != nil {
				t.Fatal(err)
			}
			seqs = append(seqs, ev.Seq)
		}
		return seqs
	}
	check := func(seqs []uint64) {
		t.Helper()
		if len(seqs) != 10 || seqs[0] != 191 || seqs[9] != 200 {
			t.Fatalf("read %v", seqs)
		}
	}
	reads := l.reads.Load()
	check(read())
	if d := l.reads.Load() - reads; d > 11 {
		t.Fatalf("a read after the cursor decoded %d lines", d)
	}
	r := l.run("run")
	for _, bad := range []int64{lineOffsets(t, l, "run")[100], 7, 1 << 30} {
		r.mu.Lock()
		r.markOff = bad
		r.mu.Unlock()
		check(read())
	}
	// A Read from the start ignores the hint.
	var n int
	for _, err := range l.Read(ctx, "run", 0) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 200 {
		t.Fatalf("a full read returned %d events", n)
	}
}

// Appends that race with the loop reach the bus once each, in order within
// each run.
func TestBusEgressConcurrentAppendsArePublishedOnce(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l, bus)
	e.a.k.wg.Add(1)
	go e.replayLoop()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("run%d", w)
			for range 50 {
				e.observe(appendReplayEvent(t, l, id, EvTurnStarted))
			}
		}()
	}
	wg.Wait()
	waitReplay(t, func() bool { return len(bus.snapshot()) >= 200 })
	close(e.a.k.drainCh)
	e.a.k.wg.Wait()
	seen := map[string]bool{}
	last := map[string]uint64{}
	for _, ev := range bus.snapshot() {
		if seen[ev.ID] {
			t.Fatalf("event %s published twice", ev.ID)
		}
		seen[ev.ID] = true
		if ev.Seq <= last[ev.Subject] {
			t.Fatalf("run %s: seq %d after %d", ev.Subject, ev.Seq, last[ev.Subject])
		}
		last[ev.Subject] = ev.Seq
	}
	if len(seen) != 200 {
		t.Fatalf("published %d events, want 200", len(seen))
	}
}

// A refusal of the bus ends the pass at the first run and keeps the order
// of the runs that wait.
func TestBusEgressRefusalEndsThePass(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := &replayBus{Bus: NewMemBus(), fail: true}
	e := testReplay(t, l, bus)
	for _, id := range []string{"a", "b", "c"} {
		e.observe(appendReplayEvent(t, l, id, EvRunFinished))
	}
	if _, err := e.pass(context.Background(), false, func() bool { return false }); err == nil {
		t.Fatal("expected the refusal")
	}
	if bus.attempts != 1 {
		t.Fatalf("the pass tried %d publications, want 1", bus.attempts)
	}
	if got := strings.Join(e.takeDirty(), ","); got != "a,b,c" {
		t.Fatalf("waiting runs = %s", got)
	}
}

// A run that fails with an error of its own waits for the next full pass
// and does not hold back the other runs.
func TestBusEgressBrokenRunDoesNotHoldBackOthers(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	appendReplayEvent(t, l, "bad", EvRunStarted)
	appendReplayEvent(t, l, "bad", EvRunFinished)
	good := appendReplayEvent(t, l, "good", EvRunFinished)
	bus := &replayBus{Bus: NewMemBus()}
	e := testReplay(t, l, bus)
	e.cursor["bad"] = 0
	// A line in the middle that is not an event is an error of the run.
	data, err := os.ReadFile(l.ledgerPath("bad"))
	if err != nil {
		t.Fatal(err)
	}
	end := strings.IndexByte(string(data), '\n') + 1
	broken := append(append(append([]byte{}, data[:end]...), "not an event\n"...), data[end:]...)
	if err := os.WriteFile(l.ledgerPath("bad"), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	e.markLocked("bad")
	e.markLocked("good")
	more, err := e.pass(context.Background(), false, func() bool { return false })
	if err != nil || more {
		t.Fatalf("pass = %v, %v", more, err)
	}
	got := bus.snapshot()
	if got[len(got)-1].ID != good.ID {
		t.Fatalf("the good run waited behind the broken one: %+v", got)
	}
	if !e.broken["bad"] || len(e.takeDirty()) != 0 {
		t.Fatal("the broken run did not wait for the next full pass")
	}
}

// The events of a shutdown, such as fw.run.suspended, go out inline after
// the replay closed.
func TestBusEgressPublishesTheShutdownInline(t *testing.T) {
	l, err := OpenFileLedger(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bus := &replayBus{Bus: NewMemBus()}
	cfg := DefaultConfig()
	cfg.Bus.Egress, cfg.Bus.EgressTypes = "fw.events", []string{"fw.run.*"}
	started := make(chan struct{}, 1)
	a := newTestAgent(t, blockingModel(started), WithConfig(cfg), WithLedger(l), WithBus(bus))
	r, err := a.Start(testCtx(t), "go")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	a.Close(ctx)
	var types []string
	for _, ev := range bus.snapshot() {
		if ev.Subject == r.ID {
			types = append(types, ev.Type)
		}
	}
	if len(types) == 0 || types[len(types)-1] != EvRunSuspended {
		t.Fatalf("published %v, want fw.run.suspended last", types)
	}
}

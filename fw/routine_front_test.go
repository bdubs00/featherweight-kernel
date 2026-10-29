package fw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeRoutines is a RoutineSource for the tests: it answers the list that
// the test sets now, or err, and counts its reads.
type fakeRoutines struct {
	mu    sync.Mutex
	list  RoutineList
	err   error
	reads int
}

func (f *fakeRoutines) set(l RoutineList, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list, f.err = l, err
}

func (f *fakeRoutines) RoutinesOf(context.Context, string) (RoutineList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.list, f.err
}

// startedRoutines returns the field routines of fw.run.started.
func startedRoutines(t *testing.T, evs []Event) *StartRoutines {
	t.Helper()
	var d RunStartedData
	firstOf(t, evs, EvRunStarted, &d)
	return d.Routines
}

// A run applies the routines that the front gives it at its start, in
// place of the routines of its profile, and the next run applies what the
// front gives it then: a move of a routine reaches the next run with no
// change of the configuration. A front that does not hold the agent leaves
// the profile its routines. A front that does not answer gives the run no
// routine, also when the profile has a live one, because a copy of the
// worker can be stale.
func TestRoutinesFromTheFront(t *testing.T) {
	ctx := testCtx(t)
	src := &fakeRoutines{}
	st := &stock{text: "STOCK pens=90"}
	s := newScript(
		// The second run: the routine rehearses, so the model works.
		callTools(use("t1", "stock.read", `{"path": "levels.txt"}`)),
		callTools(use("t2", "stock.write", `{"path": "report.txt", "text": "STOCK pens=90"}`)),
		endTurn("ACK STOCK pens=90"),
		// The fourth run: the front does not answer.
		endTurn("I did it myself."),
	)
	// The profile has a live routine of its own; the front replaces it.
	own := stockRoutine(RoutineLive)
	own.Name = "own-ack"
	prof := AgentProfile{Name: "clerk", Routines: []Routine{own}}
	a := newTestAgent(t, s.Model(), WithTools(st.tools()...), WithProfile(prof), WithRoutineSource(src))

	// The front holds the routine live, version 1.
	src.set(RoutineList{Held: true, Version: "v1", List: []Routine{stockRoutine(RoutineLive)}}, nil)
	r, err := a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if got := startedRoutines(t, evs); got == nil || got.Version != "v1" || len(got.List) != 1 || got.Error != "" {
		t.Fatalf("started routines = %+v", got)
	}
	var app RoutineAppliedData
	firstOf(t, evs, EvRoutineApplied, &app)
	if !app.OK || app.Routine != "stock-ack" || r.Output != "ACK STOCK pens=90" || len(s.requests()) != 0 {
		t.Fatalf("applied = %+v, output %q, model calls %d", app, r.Output, len(s.requests()))
	}
	checkCursorPrefixes(t, evs)

	// The routine steps back to rehearsing at the front. The next run
	// rehearses it and compares the calls of the agent with it.
	src.set(RoutineList{Held: true, Version: "v2", List: []Routine{stockRoutine(RoutineShadow)}}, nil)
	r, err = a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs = ledgerEvents(t, a.Ledger(), r.ID)
	var dec RoutineDecidedData
	firstOf(t, evs, EvRoutineDecided, &dec)
	cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
	if dec.Mode != RoutineShadow || dec.Routine != "stock-ack" || countType(evs, EvRoutineApplied) != 0 || len(cmp) != 1 || !cmp[0].Agree {
		t.Fatalf("decided = %+v, compared %+v", dec, cmp)
	}
	if got := startedRoutines(t, evs); got == nil || got.Version != "v2" {
		t.Fatalf("started routines = %+v", got)
	}
	checkCursorPrefixes(t, evs)

	// A front that does not hold the agent leaves the profile its own.
	src.set(RoutineList{Held: false, Version: "none"}, nil)
	r, err = a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs = ledgerEvents(t, a.Ledger(), r.ID)
	firstOf(t, evs, EvRoutineApplied, &app)
	if startedRoutines(t, evs) != nil || app.Routine != "own-ack" || !app.OK {
		t.Fatalf("started %+v, applied %+v", startedRoutines(t, evs), app)
	}

	// A front that does not answer gives the run no routine.
	src.set(RoutineList{}, errors.New("the front did not answer"))
	r, err = a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs = ledgerEvents(t, a.Ledger(), r.ID)
	got := startedRoutines(t, evs)
	if got == nil || len(got.List) != 0 || !strings.Contains(got.Error, "did not answer") ||
		countType(evs, EvRoutineDecided) != 0 || r.Output != "I did it myself." {
		t.Fatalf("started %+v, events %v", got, eventTypes(evs))
	}
	if src.reads != 4 {
		t.Fatalf("the front was read %d times for 4 runs", src.reads)
	}
}

// The run keeps the routines that it read at its start for its life: a
// change at the front during the run reaches the comparison at its end
// neither, so the evidence names the version that the run rehearsed. A
// list that the kernel does not accept gives the run no routine.
func TestRoutinesOfTheStartHoldForTheRun(t *testing.T) {
	ctx := testCtx(t)
	src := &fakeRoutines{}
	st := &stock{text: "STOCK pens=90"}
	flip := NewTool(ToolSpec{Name: "flip"}, func(context.Context, json.RawMessage) (ToolResult, error) {
		next := stockRoutine(RoutineShadow)
		next.Version = 2
		src.set(RoutineList{Held: true, Version: "v2", List: []Routine{next}}, nil)
		return TextResult("flipped"), nil
	})
	s := newScript(
		callTools(use("t1", "stock.read", `{"path": "levels.txt"}`), use("f1", "flip", `{}`)),
		callTools(use("t2", "stock.write", `{"path": "report.txt", "text": "STOCK pens=90"}`)),
		endTurn("ACK STOCK pens=90"),
		endTurn("I did it myself."),
	)
	a := newTestAgent(t, s.Model(), WithTools(append(st.tools(), flip)...), WithProfile(AgentProfile{Name: "clerk"}), WithRoutineSource(src))
	src.set(RoutineList{Held: true, Version: "v1", List: []Routine{stockRoutine(RoutineShadow)}}, nil)
	r, err := a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	cmp := decodeAll[RoutineComparedData](t, evs, EvRoutineCompared)
	if len(cmp) != 1 || cmp[0].Version != 1 {
		t.Fatalf("compared = %+v", cmp)
	}

	// A routine with a question names a decider that this process lacks.
	bad := stockRoutine(RoutineLive)
	bad.Decider = "clef"
	bad.Questions = map[string]RoutineQuestion{"kind": {Question: Question{Type: QuestionNoul}, Act: 0.9}}
	src.set(RoutineList{Held: true, Version: "v3", List: []Routine{bad}}, nil)
	r, err = a.Run(ctx, "Acknowledge the stock file.", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs = ledgerEvents(t, a.Ledger(), r.ID)
	if got := startedRoutines(t, evs); got == nil || got.Version != "v3" || len(got.List) != 0 || got.Error == "" || countType(evs, EvRoutineDecided) != 0 {
		t.Fatalf("started %+v, events %v", got, eventTypes(evs))
	}
}

// The client of routines.front sends the static token and the version of
// the list that it holds. The front answers 304 when that list is
// current, and the client gives it; a new version replaces it. Any other
// answer is an error that names neither the address nor the token.
func TestFrontRoutinesClient(t *testing.T) {
	t.Setenv("FW_TEST_FRONT_TOKEN", "s3cret-token")
	var mu sync.Mutex
	version, list := "v1", []Routine{stockRoutine(RoutineLive)}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Get("If-None-Match"))
		if r.URL.Path != RoutinesPath || r.URL.Query().Get("agent") != "clerk" || r.Header.Get("Authorization") != "Bearer s3cret-token" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if version == "broken" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", strconv.Quote(version))
		if r.Header.Get("If-None-Match") == strconv.Quote(version) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(RoutineList{Held: true, Version: version, List: list})
	}))
	defer srv.Close()
	f, err := newFrontRoutines(&MemoryFrontConfig{URL: srv.URL + "/", TokenEnv: "FW_TEST_FRONT_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testCtx(t)
	for i, want := range []string{"v1", "v1", "v2"} {
		if i == 2 {
			mu.Lock()
			version, list = "v2", []Routine{stockRoutine(RoutineShadow)}
			mu.Unlock()
		}
		got, err := f.RoutinesOf(ctx, "clerk")
		if err != nil || !got.Held || got.Version != want || len(got.List) != 1 {
			t.Fatalf("read %d = %+v, %v", i, got, err)
		}
	}
	if seen[0] != "" || seen[1] != `"v1"` || seen[2] != `"v1"` {
		t.Fatalf("If-None-Match = %q", seen)
	}
	mu.Lock()
	version = "broken"
	mu.Unlock()
	if _, err := f.RoutinesOf(ctx, "clerk"); err == nil || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
	if _, err := newFrontRoutines(&MemoryFrontConfig{URL: "fw-front:8080", TokenEnv: "X"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a bad address: %v", err)
	}

	// A process with routines.front reads through the client and keeps
	// the token out of every event.
	cfg := DefaultConfig()
	cfg.Routines.Front = &MemoryFrontConfig{URL: srv.URL, TokenEnv: "FW_TEST_FRONT_TOKEN"}
	mu.Lock()
	version, list = "v3", []Routine{stockRoutine(RoutineLive)}
	mu.Unlock()
	a := newTestAgent(t, newScript().Model(), WithConfig(cfg), WithTools((&stock{text: "STOCK pens=90"}).tools()...), WithProfile(AgentProfile{Name: "clerk"}))
	r, err := a.Run(ctx, "Acknowledge the stock file. s3cret-token", AsAgent("clerk"))
	mustState(t, r, err, StateCompleted)
	evs := ledgerEvents(t, a.Ledger(), r.ID)
	if got := startedRoutines(t, evs); got == nil || got.Version != "v3" || countType(evs, EvRoutineApplied) != 1 {
		t.Fatalf("started %+v", got)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), "s3cret-token") {
			t.Fatalf("an event holds the token: %s", e.Type)
		}
	}
}

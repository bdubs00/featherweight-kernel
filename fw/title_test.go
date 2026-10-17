package fw

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// titleModel answers a title call with title and every other call with
// "done".
type titleModel struct {
	title string
	mu    sync.Mutex
	calls int
}

func (m *titleModel) Generate(_ context.Context, req *Request) (*Response, error) {
	if req.System == titleSystem {
		m.mu.Lock()
		m.calls++
		m.mu.Unlock()
		return &Response{Content: []Block{Text(m.title)}, StopReason: StopEndTurn, Usage: Usage{InputTokens: 30, OutputTokens: 4}, Provider: "fake", Model: "t"}, nil
	}
	return endTurn("done"), nil
}

func (m *titleModel) Stream(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
	return m.Generate(ctx, req)
}

func titledAgent(t *testing.T, m Model, titles string) *Agent {
	a := newTestAgent(t, m)
	a.cfg.Titles = titles
	return a
}

func waitTitle(t *testing.T, a *Agent, id, want string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := a.Get(context.Background(), id)
		if err == nil && r.Title == want {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, _ := a.Get(context.Background(), id)
	t.Fatalf("title = %q, want %q", r.Title, want)
	return nil
}

func TestModelTitle(t *testing.T) {
	ctx := testCtx(t)
	m := &titleModel{title: "Title: \"Fix checkout retries.\""}
	a := titledAgent(t, m, TitlesModel)
	r, err := a.Run(ctx, "Investigate the intermittent checkout failure in the payments service, which started on Tuesday")
	mustState(t, r, err, StateCompleted)
	got := waitTitle(t, a, r.ID, "Fix checkout retries")
	if got.Usage.InputTokens < 30 {
		t.Fatalf("the title call is not in the usage: %+v", got.Usage)
	}
	var d RunTitledData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunTitled, &d)
	if d.Title != "Fix checkout retries" || d.Usage == nil || d.Provider != "fake" {
		t.Fatalf("titled = %+v", d)
	}
}

func TestModelTitleSkips(t *testing.T) {
	ctx := testCtx(t)
	m := &titleModel{title: "Never"}
	a := titledAgent(t, m, TitlesModel)
	r, err := a.Run(ctx, "Do the work", WithMetadata(map[string]string{MetaTitle: "Mine"}))
	mustState(t, r, err, StateCompleted)
	off := titledAgent(t, m, TitlesInput)
	r2, err := off.Run(ctx, "Do the other work")
	mustState(t, r2, err, StateCompleted)
	time.Sleep(50 * time.Millisecond)
	if m.calls != 0 || r.Title != "Mine" || r2.Title != "Do the other work" {
		t.Fatalf("calls = %d, titles %q %q", m.calls, r.Title, r2.Title)
	}
}

func TestCleanTitle(t *testing.T) {
	cases := map[string]string{
		"Fix checkout retries":                  "Fix checkout retries",
		"**Fix checkout retries.**":             "Fix checkout retries",
		"Sure.\n\nTitle: Release notes for 2.4": "Release notes for 2.4",
		"# “Rotate the keys”":                   "Rotate the keys",
		"   ":                                   "",
		"a​b":                                   "ab",
	}
	for in, want := range cases {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanTitle(strings.Repeat("word ", 40)); len([]rune(got)) > MaxTitleRunes+1 {
		t.Errorf("long title %q", got)
	}
}

// blockModel answers a title call only when its context ends.
type blockModel struct{}

func (blockModel) Generate(ctx context.Context, req *Request) (*Response, error) {
	if req.System == titleSystem {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return endTurn("done"), nil
}

func (m blockModel) Stream(ctx context.Context, req *Request, _ func(Delta)) (*Response, error) {
	return m.Generate(ctx, req)
}

func TestModelTitleDoesNotHoldShutdown(t *testing.T) {
	ctx := testCtx(t)
	a := titledAgent(t, blockModel{}, TitlesModel)
	r, err := a.Run(ctx, "Do the work")
	mustState(t, r, err, StateCompleted)
	began := time.Now()
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	a.Close(closeCtx)
	if d := time.Since(began); d > 5*time.Second {
		t.Fatalf("Close waited %s for a title call", d)
	}
}

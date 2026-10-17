package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fakeTokens is the PersonTokens of a test. It knows the people who
// connected the service tickets.
type fakeTokens struct {
	mu        sync.Mutex
	connected map[string]bool
}

func newFakeTokens() *fakeTokens { return &fakeTokens{connected: map[string]bool{}} }

func (f *fakeTokens) connect(subject string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected[subject] = true
}

func (f *fakeTokens) Token(_ context.Context, subject, server string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if server != "tickets" || !f.connected[subject] {
		return "", ErrNotConnected
	}
	return "at-" + subject, nil
}

func (f *fakeTokens) Reject(context.Context, string, string, string) {}

func (f *fakeTokens) Forget(_ context.Context, subject, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.connected, subject)
}

// ticketsSource is a tool source with one tool that acts for the person of
// the run. Without a token of the person, the tool asks the person to
// connect, as the MCP client does.
type ticketsSource struct {
	mu    sync.Mutex
	infos []RunInfo
}

func (f *ticketsSource) Tools(context.Context) ([]Tool, error) {
	return []Tool{NewTool(ToolSpec{Name: "tickets.list", Source: "tickets"}, func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
		info := RunInfoFrom(ctx)
		f.mu.Lock()
		f.infos = append(f.infos, info)
		f.mu.Unlock()
		if info.PrincipalKind != KindUser {
			return ErrorResult("tickets acts for a person"), nil
		}
		tokens := AgentFrom(ctx).PersonTokens()
		if tokens == nil {
			return ErrorResult("this process keeps no connections of people"), nil
		}
		if _, err := tokens.Token(ctx, info.Principal, "tickets"); errors.Is(err, ErrNotConnected) {
			if err := RequestConnect(ctx, ConnectRequest{Server: "tickets", Title: "Tickets", Reason: ConnectMissing}); err != nil {
				return ToolResult{}, err
			}
		}
		if _, err := tokens.Token(ctx, info.Principal, "tickets"); err != nil {
			return ErrorResult(err.Error()), nil
		}
		return TextResult("tickets of " + info.Principal), nil
	})}, nil
}

func TestConnectPausesAndResumes(t *testing.T) {
	ctx := testCtx(t)
	ada := &Principal{Subject: "ada", Kind: KindUser}
	pctx := WithPrincipal(ctx, ada)
	tokens := newFakeTokens()
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(&ticketsSource{}))
	a.SetPersonTokens(tokens)
	if a.PersonTokens() != tokens {
		t.Fatal("PersonTokens did not return the tokens that SetPersonTokens set")
	}

	r, err := a.Run(pctx, "list my tickets")
	mustState(t, r, err, StateInputRequired)
	if r.Pending.Connect == nil || r.Pending.Connect.Server != "tickets" || r.Pending.Reason != (ConnectRequest{Title: "Tickets"}).Message() {
		t.Fatalf("pending = %+v", r.Pending)
	}
	// A click is not a grant.
	err = a.Decide(pctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "connect Tickets") {
		t.Fatalf("Decide allow = %v", err)
	}
	// A connection of another service, or of another person, resumes nothing.
	if n := a.Connected("ada", "docs"); n != 0 {
		t.Fatalf("Connected(docs) resumed %d runs", n)
	}
	if n := a.Connected("bob", "tickets"); n != 0 {
		t.Fatalf("Connected(bob) resumed %d runs", n)
	}
	tokens.connect("ada")
	if n := a.Connected("ada", "tickets"); n != 1 {
		t.Fatalf("Connected resumed %d runs, want 1", n)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := TextOf(lastToolResults(t, s.requests()[1])[0].Content); got != "tickets of ada" {
		t.Fatalf("tool result = %q", got)
	}
	var d ApprovalDecidedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvApprovalDecided, &d)
	if d.Decision != "allow" || d.Decider != "ada" || d.Note != "connected" {
		t.Fatalf("decision = %+v", d)
	}
	// The ledger holds no token.
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		if bytes.Contains(e.Data, []byte("at-ada")) {
			t.Fatalf("event %s holds the token", e.Type)
		}
	}
	if n := a.Connected("ada", "tickets"); n != 0 {
		t.Fatalf("a second Connected resumed %d runs", n)
	}
}

func TestConnectSkip(t *testing.T) {
	ctx := testCtx(t)
	pctx := WithPrincipal(ctx, &Principal{Subject: "ada", Kind: KindUser})
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(&ticketsSource{}))
	a.SetPersonTokens(newFakeTokens())
	r, err := a.Run(pctx, "list my tickets")
	mustState(t, r, err, StateInputRequired)
	if err := a.Decide(pctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "deny", Note: "not now"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])[0]
	if !res.IsError || !strings.Contains(TextOf(res.Content), "did not connect") {
		t.Fatalf("tool result = %+v", res)
	}
}

func TestConnectWithoutPersonTokens(t *testing.T) {
	ctx := testCtx(t)
	pctx := WithPrincipal(ctx, &Principal{Subject: "ada", Kind: KindUser})
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(&ticketsSource{}))
	if a.PersonTokens() != nil {
		t.Fatal("an agent has tokens of people before any package set them")
	}
	r, err := a.Run(pctx, "list my tickets")
	mustState(t, r, err, StateCompleted)
	res := lastToolResults(t, s.requests()[1])[0]
	if !res.IsError || !strings.Contains(TextOf(res.Content), "no connections of people") {
		t.Fatalf("tool result = %+v", res)
	}
}

func TestRunInfoCarriesThePerson(t *testing.T) {
	ctx := testCtx(t)
	fc := &ticketsSource{}
	s := newScript(callTools(use("c1", "tickets.list", `{}`)), endTurn("done"))
	a := newTestAgent(t, s.Model(), WithToolSources(fc))
	a.SetPersonTokens(newFakeTokens())
	r, err := a.Run(WithPrincipal(ctx, &Principal{Subject: "token", Kind: KindToken}), "list")
	mustState(t, r, err, StateCompleted)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.infos) != 1 || fc.infos[0].Principal != "token" || fc.infos[0].PrincipalKind != KindToken || fc.infos[0].RunID != r.ID {
		t.Fatalf("run info = %+v", fc.infos)
	}
}

// seedConnectWait leaves a run as a process leaves it when it stops while
// a call of ada waits for a connection.
func seedConnectWait(t *testing.T, l Ledger, id string) {
	seed(t, l, id,
		MustEvent(id, EvRunQueued, RunQueuedData{Agent: "default", Input: "hi", Origin: OriginCLI, Principal: "ada", PrincipalKind: KindUser}),
		MustEvent(id, EvRunStarted, RunStartedData{}),
		MustEvent(id, EvTurnStarted, TurnData{Turn: 1}),
		MustEvent(id, EvModelRequested, ModelRequestedData{}),
		MustEvent(id, EvModelResponded, ModelRespondedData{Content: []Block{use("c1", "tickets.list", `{}`)}, StopReason: StopToolUse}),
		MustEvent(id, EvToolRequested, ToolRequestedData{Tool: "tickets.list", CallID: "c1", Input: json.RawMessage(`{}`)}),
		MustEvent(id, EvApprovalRequested, ApprovalRequestedData{ApprovalID: "ap1", Tool: "tickets.list", CallID: "c1",
			Reason: "Connect Tickets", Connect: &ConnectRequest{Server: "tickets", Title: "Tickets", Reason: ConnectMissing}}),
	)
}

func TestConnectSurvivesARestart(t *testing.T) {
	ctx := testCtx(t)
	t.Run("connect after the restart runs the call once", func(t *testing.T) {
		l := NewMemLedger()
		seedConnectWait(t, l, "r1")
		s := newScript(endTurn("done"))
		tokens := newFakeTokens()
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolSources(&ticketsSource{}))
		a.SetPersonTokens(tokens)
		r, err := a.Get(ctx, "r1")
		mustState(t, r, err, StateInputRequired)
		if r.Pending.Connect == nil || r.Pending.ID != "ap1" {
			t.Fatalf("pending = %+v", r.Pending)
		}
		tokens.connect("ada")
		if n := a.Connected("ada", "tickets"); n != 1 {
			t.Fatalf("Connected resumed %d runs", n)
		}
		r, err = a.Wait(ctx, "r1")
		mustState(t, r, err, StateCompleted)
		evs := ledgerEvents(t, a.Ledger(), "r1")
		// No "rerun after resume" question: the service never saw the call.
		if n := countType(evs, EvApprovalRequested); n != 1 {
			t.Fatalf("approvals = %d, want 1", n)
		}
		if got := TextOf(lastToolResults(t, s.requests()[0])[0].Content); got != "tickets of ada" {
			t.Fatalf("tool result = %q", got)
		}
	})
	t.Run("a skip before the restart ends the call", func(t *testing.T) {
		l := NewMemLedger()
		seedConnectWait(t, l, "r2")
		seed(t, l, "r2", MustEvent("r2", EvApprovalDecided, ApprovalDecidedData{ApprovalID: "ap1", Decision: "deny", Decider: "ada"}))
		s := newScript(endTurn("done"))
		a := newTestAgent(t, s.Model(), WithLedger(l), WithToolSources(&ticketsSource{}))
		a.SetPersonTokens(newFakeTokens())
		r, err := a.Wait(ctx, "r2")
		mustState(t, r, err, StateCompleted)
		res := lastToolResults(t, s.requests()[0])[0]
		if !res.IsError || !strings.Contains(TextOf(res.Content), "chose not to") {
			t.Fatalf("tool result = %+v", res)
		}
	})
}

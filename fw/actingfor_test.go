package fw

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// WithActingFor names a person only for a trusted caller: the process
// itself, the static token, or a workload. A person who names another
// person is refused, its own subject and an empty subject change nothing,
// a bad subject is an invalid request, and the person holds only while the
// caller that named it is the principal.
func TestWithActingFor(t *testing.T) {
	ctx := testCtx(t)
	token := WithPrincipal(ctx, &Principal{Subject: KindToken, Kind: KindToken})
	workload := WithPrincipal(ctx, &Principal{Subject: "spiffe://x/front", Kind: KindWorkload})
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	anonymous := WithPrincipal(ctx, &Principal{Subject: KindAnonymous, Kind: KindAnonymous})
	for name, caller := range map[string]context.Context{"process": ctx, "token": token, "workload": workload} {
		got, err := WithActingFor(caller, " ada ")
		if err != nil || ActingFor(got) != "ada" || personOf(got) != "ada" || PrincipalFrom(got) != PrincipalFrom(caller) {
			t.Errorf("%s: acting for %q, person %q, principal %+v, %v", name, ActingFor(got), personOf(got), PrincipalFrom(got), err)
		}
	}
	for name, caller := range map[string]context.Context{"alice": alice, "anonymous": anonymous} {
		if _, err := WithActingFor(caller, "bob"); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s acted for another person: %v", name, err)
		}
	}
	for _, s := range []string{"", "  ", "alice"} {
		got, err := WithActingFor(alice, s)
		if err != nil || got != alice || ActingFor(got) != "" || personOf(got) != "alice" {
			t.Errorf("alice with %q: acting for %q, person %q, %v", s, ActingFor(got), personOf(got), err)
		}
	}
	if got, err := WithActingFor(token, strings.Repeat("x", MaxActingForBytes)); err != nil || ActingFor(got) == "" {
		t.Fatalf("a subject at the bound: %v", err)
	}
	for _, s := range []string{strings.Repeat("x", MaxActingForBytes+1), "two\nlines", "nul\x00", "bad\xff"} {
		if _, err := WithActingFor(token, s); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("WithActingFor(%q) = %v, want ErrInvalidRequest", s, err)
		}
	}
	// The person belongs to the caller that named it, so another principal
	// in the same context acts for itself.
	relayed, err := WithActingFor(token, "ada")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Principal{{Subject: "bob", Kind: KindUser}, {Subject: KindToken, Kind: KindToken}, nil} {
		if got := WithPrincipal(relayed, p); ActingFor(got) != "" {
			t.Errorf("the principal %+v kept the person %q", p, ActingFor(got))
		}
	}
}

// A trusted caller that acts for a person records that person as the
// decider of an approval, as the sender of a follow-up, as the person of
// a change of a budget, of a reaction, and of a cancel, and as the person
// of a branch. The kernel still authorizes the caller alone: the token
// passes for a person without a role, a workload without a role fails for
// an admin, and a person who names itself changes nothing.
func TestActingForRecordsThePerson(t *testing.T) {
	ctx := testCtx(t)
	cfg := DefaultConfig()
	cfg.Access.Default = "closed"
	cfg.Access.Admins = []string{"kind:token", "alice"}
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{"path":"x"}`)), endTurn("done"), endTurn("again"), endTurn("as the token"),
		callTools(use("c2", "rm", `{"path":"y"}`)))
	a := newTestAgent(t, s.Model(), WithConfig(cfg), WithTools(destructiveTool(&calls)))
	token := WithPrincipal(ctx, &Principal{Subject: KindToken, Kind: KindToken})
	// Ada has no role on the agent. The token acts for her.
	relayed, err := WithActingFor(token, "ada")
	if err != nil {
		t.Fatal(err)
	}

	r, err := a.Run(ctx, "clean up")
	mustState(t, r, err, StateInputRequired)
	if err := a.Decide(relayed, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if err := a.Send(relayed, r.ID, UserText("again"), OriginHTTP, WithSendBudget(Budget{MaxTurns: 9})); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if _, err := a.ChangeBudget(relayed, r.ID, Budget{MaxTurns: 10}, OriginHTTP); err != nil {
		t.Fatal(err)
	}
	if err := a.React(relayed, r.ID, ReactionData{Reaction: ReactionHeart}); err != nil {
		t.Fatal(err)
	}
	branch, err := a.Fork(relayed, r.ID, ForkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Without a person, the token is the one that the records name.
	if err := a.Send(token, r.ID, UserText("as the token"), OriginHTTP); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)

	var decided ApprovalDecidedData
	var budgets []BudgetChangedData
	for _, e := range ledgerEvents(t, a.Ledger(), r.ID) {
		switch e.Type {
		case EvApprovalDecided:
			_ = e.Decode(&decided)
		case EvBudgetChanged:
			var d BudgetChangedData
			_ = e.Decode(&d)
			budgets = append(budgets, d)
		}
	}
	if decided.Decider != "ada" || decided.Decision != "allow" || calls.Load() != 1 {
		t.Fatalf("decided = %+v, %d calls", decided, calls.Load())
	}
	if msgs := receivedOf(t, a.Ledger(), r.ID); len(msgs) != 2 || msgs[0].By != "ada" || msgs[0].From != nil || msgs[1].By != KindToken {
		t.Fatalf("messages = %+v", msgs)
	}
	if len(budgets) != 2 || budgets[0].By != "ada" || budgets[0].Budget.MaxTurns != 9 || budgets[1].By != "ada" || budgets[1].Budget.MaxTurns != 10 {
		t.Fatalf("budget changes = %+v", budgets)
	}
	if got := reactionsOf(t, a.Ledger(), r.ID); len(got) != 1 || got[0].By != "ada" {
		t.Fatalf("reactions = %+v", got)
	}
	if q := queuedOf(t, a.Ledger(), branch.ID); q.Principal != "ada" || q.PrincipalKind != KindUser || branch.Principal != "ada" {
		t.Fatalf("branch = %+v, queued principal %q of kind %q", branch, q.Principal, q.PrincipalKind)
	}

	// The second run waits for an approval. A workload without a role is
	// refused, also when it acts for an admin, and so is a person without
	// a role who names itself.
	r2, err := a.Run(ctx, "clean up again")
	mustState(t, r2, err, StateInputRequired)
	front := WithPrincipal(ctx, &Principal{Subject: "spiffe://x/front", Kind: KindWorkload})
	forAlice, err := WithActingFor(front, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := WithActingFor(WithPrincipal(ctx, &Principal{Subject: "bob", Kind: KindUser}), "bob")
	if err != nil {
		t.Fatal(err)
	}
	for name, caller := range map[string]context.Context{"a workload for alice": forAlice, "bob for himself": bob} {
		if err := a.Decide(caller, r2.ID, ApprovalDecision{ApprovalID: r2.Pending.ID, Decision: "allow"}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s decided: %v", name, err)
		}
		if err := a.Send(caller, r.ID, UserText("hi"), OriginHTTP); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s sent: %v", name, err)
		}
		if _, err := a.ChangeBudget(caller, r.ID, Budget{MaxTurns: 11}, OriginHTTP); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s changed the budget: %v", name, err)
		}
		if err := a.React(caller, r.ID, ReactionData{Reaction: ReactionUp}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s reacted: %v", name, err)
		}
		if _, err := a.Fork(caller, r.ID, ForkRequest{}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s forked: %v", name, err)
		}
		if err := a.Cancel(caller, r2.ID); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s canceled: %v", name, err)
		}
	}
	if err := a.Cancel(relayed, r2.ID); err != nil {
		t.Fatal(err)
	}
	r2, err = a.Wait(ctx, r2.ID)
	mustState(t, r2, err, StateCanceled)
	var fin RunFinishedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r2.ID), EvRunFinished, &fin)
	if fin.Outcome != OutcomeCanceled || fin.By != "ada" || countType(ledgerEvents(t, a.Ledger(), r2.ID), EvApprovalDecided) != 0 {
		t.Fatalf("finished = %+v", fin)
	}
	if n := len(reactionsOf(t, a.Ledger(), r.ID)); n != 1 {
		t.Fatalf("a refused reaction is in the ledger: %d reactions", n)
	}
}

// A cancel records who canceled the run: the person in the context, and
// nobody for the process itself.
func TestCancelRecordsThePerson(t *testing.T) {
	ctx := testCtx(t)
	var calls atomic.Int32
	s := newScript(callTools(use("c1", "rm", `{"path":"x"}`)), callTools(use("c2", "rm", `{"path":"y"}`)))
	a := newTestAgent(t, s.Model(), WithTools(destructiveTool(&calls)))
	alice := WithPrincipal(ctx, &Principal{Subject: "alice", Kind: KindUser})
	for _, tc := range []struct {
		ctx  context.Context
		want string
	}{{alice, "alice"}, {ctx, ""}} {
		r, err := a.Run(tc.ctx, "clean up")
		mustState(t, r, err, StateInputRequired)
		if err := a.Cancel(tc.ctx, r.ID); err != nil {
			t.Fatal(err)
		}
		var fin RunFinishedData
		firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunFinished, &fin)
		if fin.Outcome != OutcomeCanceled || fin.Error != "canceled" || fin.By != tc.want {
			t.Errorf("finished = %+v, want by %q", fin, tc.want)
		}
	}
}

package fw

// These tests check the text of a profile: the system prompt, the
// instructions, and the description never enter an event, the model reads
// them as the configuration holds them, and a change of them after a
// restart is recorded on the run. They use the same API as the commit that
// first resolved the profile of a run, so they also show its faults.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// digestOfText is the digest of a text as ResolvedProfile describes it.
func digestOfText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// eventsHold fails the test when an event of the run holds one of texts.
func eventsHold(t *testing.T, l Ledger, run string, texts ...string) {
	t.Helper()
	for _, e := range ledgerEvents(t, l, run) {
		for _, text := range texts {
			if strings.Contains(string(e.Data), text) {
				t.Errorf("%s of %s holds %q: %.200s", e.Type, run, text, e.Data)
			}
		}
	}
}

// The model reads the text of a profile as the configuration holds it,
// before and after a restart: text with the shape of a secret, a word that
// is also the value of a secret variable, and an expanded secret stay as
// they are. None of it enters an event.
func TestModelGetsTheConfiguredText(t *testing.T) {
	t.Setenv("POSTGRES_PASSWORD", "password")
	t.Setenv("SERVICE_API_KEY", "svc-2f9a7c1e88d4")
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	prompt := "You review pull requests for leaked credentials. " +
		"Flag an AWS access key id such as AKIAIOSFODNN7EXAMPLE. " +
		"Flag a JWT such as eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U. " +
		"Our API accepts bearer authentication/authorization headers. " +
		"Clone with https://ci-bot@git.example.com/org/repo.git. Never print a password."
	instr := "Send the header X-Api-Key: svc-2f9a7c1e88d4 with every call to the billing service."
	profiles := func() map[string]AgentProfile {
		return map[string]AgentProfile{"reviewer": {System: prompt, Instructions: []string{instr}}}
	}
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Agents = profiles()
	a := pinAgent(t, cfg, pinLedger(t, dir), rec)
	rec.script(endTurn("ok"))
	r, err := a.Run(ctx, "review", AsAgent("reviewer"))
	mustState(t, r, err, StateCompleted)
	want := prompt + "\n\n# Instructions\n\n" + instr
	if got := rec.call(0).system; !strings.HasPrefix(got, want) {
		t.Fatalf("system prompt of the model = %q\nwant %q", got, want)
	}
	closePin(a)

	cfg2 := pinConfig(t, "pintest/base")
	cfg2.Agents = profiles()
	b := pinAgent(t, cfg2, pinLedger(t, dir), rec)
	rec.script(endTurn("again"))
	if err := b.Send(ctx, r.ID, UserText("again"), OriginCLI); err != nil {
		t.Fatal(err)
	}
	r, err = b.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	if got := rec.call(1).system; !strings.HasPrefix(got, want) {
		t.Fatalf("system prompt of the model after the restart = %q\nwant %q", got, want)
	}
	if n := len(driftNotes(t, b.Ledger(), r.ID)); n != 0 {
		t.Fatalf("%d records of a change, but the text did not change", n)
	}
	eventsHold(t, b.Ledger(), r.ID, "AKIAIOSFODNN7EXAMPLE", "svc-2f9a7c1e88d4", "ci-bot", "leaked credentials")
}

// No configured text enters an event: not the system prompt, the
// instructions, or the description of a profile, not the default system
// prompt, and not in a branch or a child either.
func TestNoConfiguredTextInEvents(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	l := NewMemLedger()
	a := pinAgent(t, pinConfig(t, "pintest/base"), l, rec)
	hook := "https://hooks.slack.com/services/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"
	setProfiles(t, a, map[string]AgentProfile{
		"notifier": {Description: "DESCRIPTION-MARKER", System: "Post the summary to " + hook + " when you finish.",
			Instructions: []string{"INSTRUCTION-MARKER"}},
		"plain": {Description: "PLAIN-MARKER"},
	})
	rec.script(endTurn("ok"), endTurn("plain"), endTurn("branch"), endTurn("child"))
	r, err := a.Run(ctx, "work", AsAgent("notifier"))
	mustState(t, r, err, StateCompleted)
	p, err := a.Run(ctx, "work", AsAgent("plain"))
	mustState(t, p, err, StateCompleted)
	b, err := a.Fork(ctx, r.ID, ForkRequest{Input: "again"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	c, err := a.SpawnChild(ctx, p.ID, NewRunRequest("help", AsAgent("notifier")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := rec.call(2).system; !strings.HasPrefix(got, "Post the summary to "+hook) {
		t.Fatalf("system prompt of the branch = %q", got)
	}
	for _, id := range []string{r.ID, p.ID, b.ID, c.ID} {
		eventsHold(t, l, id, "xQ9zLk2mN4pR7sT1uV3wY5aB", "DESCRIPTION-MARKER", "INSTRUCTION-MARKER", "PLAIN-MARKER",
			"You are a featherweight agent")
	}
}

// A person who can operate the agent lead but cannot view the agent helper
// does not read the text of helper in the ledger of a child that a run of
// lead spawned.
func TestChildLedgerHidesTheProfileText(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	cfg := pinConfig(t, "pintest/base")
	cfg.Access = AccessConfig{Default: "closed"}
	a := pinAgent(t, cfg, NewMemLedger(), rec)
	setProfiles(t, a, map[string]AgentProfile{
		"lead": {Access: &Access{Operators: []string{"bob"}}},
		"helper": {System: "HELPER-CONFIDENTIAL: escalate refunds over 500 to legal.",
			Instructions: []string{"HELPER-RULE"}, Access: &Access{Owners: []string{"alice"}}},
	})
	bob := WithPrincipal(ctx, &Principal{Subject: "bob"})
	if err := a.Authorize(bob, ActionAgentView, Target{Agent: "helper"}); err == nil {
		t.Fatal("bob can view helper; the test needs a profile that he cannot view")
	}
	rec.script(endTurn("led"))
	p, err := a.Run(bob, "lead", AsAgent("lead"))
	mustState(t, p, err, StateCompleted)
	// The spawn_task tool calls SpawnChild with the context of the tool,
	// which holds no principal.
	rec.script(endTurn("helped"))
	c, err := a.SpawnChild(context.Background(), p.ID, NewRunRequest("help", AsAgent("helper")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Wait(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if got := rec.call(1).system; !strings.HasPrefix(got, "HELPER-CONFIDENTIAL") {
		t.Fatalf("system prompt of the child = %q", got)
	}
	ch, err := a.Events(bob, c.ID, 0)
	if err != nil {
		t.Fatalf("bob cannot read the events of the child: %v", err)
	}
	timeout := time.After(time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			if strings.Contains(string(e.Data), "HELPER-CONFIDENTIAL") || strings.Contains(string(e.Data), "HELPER-RULE") {
				t.Fatalf("bob reads the text of helper in %s of the child: %.160s", e.Type, e.Data)
			}
		case <-timeout:
			return
		}
	}
}

// The kernel reads the profile set once for a request, so a SetProfiles in
// the middle of the start cannot split the run between two versions.
func TestStartReadsTheProfileOnce(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	var a *Agent
	var armed atomic.Bool
	// The kernel lists the tools during the start, so this source changes
	// the profile there.
	src := ToolSourceFunc(func(context.Context) ([]Tool, error) {
		if armed.CompareAndSwap(true, false) {
			if err := a.SetProfiles(map[string]AgentProfile{"worker": {Model: "pintest/new", Tools: []string{"alpha", "beta"}}}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	a = pinAgent(t, pinConfig(t, ""), NewMemLedger(), rec, WithToolSources(src))
	setProfiles(t, a, map[string]AgentProfile{"worker": {Model: "pintest/old", Tools: []string{"alpha"}}})
	armed.Store(true)
	rec.script(endTurn("ok"))
	r, err := a.Run(ctx, "work", AsAgent("worker"), WithRunProfile(AgentProfile{Tools: []string{"alpha", "beta"}}))
	mustState(t, r, err, StateCompleted)
	if armed.Load() {
		t.Fatal("the start listed no tools, so the test changed nothing")
	}
	var q RunQueuedData
	firstOf(t, ledgerEvents(t, a.Ledger(), r.ID), EvRunQueued, &q)
	if q.Resolved == nil || q.Resolved.Model != "pintest/old" || !reflect.DeepEqual(q.ExtraTools, []string{"beta"}) {
		t.Fatalf("queued = model %+v, extra tools %v; want the old profile in both", q.Resolved, q.ExtraTools)
	}
	if c := rec.call(0); c.model != "old" {
		t.Fatalf("the run uses the model %q", c.model)
	}
}

// After a restart, a run whose profile text changed goes on with the text
// now, and the kernel records the change once on the run, with the field
// and the old and the new digest. The same text records nothing.
func TestTextChangeAfterRestartIsRecorded(t *testing.T) {
	ctx := testCtx(t)
	dir := filepath.Join(t.TempDir(), "runs")
	rec := &pinRecorder{}
	agentsWith := func(system string) map[string]AgentProfile {
		return map[string]AgentProfile{"worker": {System: system, Instructions: []string{"Keep it short."}}}
	}
	start := func(system string) *Agent {
		cfg := pinConfig(t, "pintest/base")
		cfg.Agents = agentsWith(system)
		return pinAgent(t, cfg, pinLedger(t, dir), rec)
	}
	follow := func(a *Agent, id string) {
		t.Helper()
		rec.script(endTurn("more"))
		if err := a.Send(ctx, id, UserText("more"), OriginCLI); err != nil {
			t.Fatal(err)
		}
		r, err := a.Wait(ctx, id)
		mustState(t, r, err, StateCompleted)
	}
	tools := "alpha,beta,gamma,delta"

	a := start("Old system.")
	rec.script(endTurn("one"))
	r, err := a.Run(ctx, "first", AsAgent("worker"))
	mustState(t, r, err, StateCompleted)
	closePin(a)

	b := start("Old system.")
	follow(b, r.ID)
	rec.expect(t, 1, 2, "base", "Old system.", tools)
	if n := len(driftNotes(t, b.Ledger(), r.ID)); n != 0 {
		t.Fatalf("%d records of a change, but the text did not change", n)
	}
	closePin(b)

	c := start("New system.")
	follow(c, r.ID)
	rec.expect(t, 2, 3, "base", "New system.", tools)
	notes := driftNotes(t, c.Ledger(), r.ID)
	want := map[string]string{"fw.drift.system": digestOfText("Old system.") + " -> " + digestOfText("New system.")}
	if len(notes) != 1 || !reflect.DeepEqual(notes[0], want) {
		t.Fatalf("records of the change = %v, want one %v", notes, want)
	}
	if got, _ := c.Get(ctx, r.ID); got.Metadata["fw.drift.system"] != want["fw.drift.system"] {
		t.Fatalf("metadata of the run = %v", got.Metadata)
	}
	follow(c, r.ID)
	closePin(c)

	d := start("New system.")
	follow(d, r.ID)
	rec.expect(t, 4, 5, "base", "New system.", tools)
	if n := len(driftNotes(t, d.Ledger(), r.ID)); n != 1 {
		t.Fatalf("%d records of the change, want still 1", n)
	}
}

// A deny that a person adds to the profile while a call waits for its
// approval wins over the allow, and the result of the call says so.
func TestDenyAddedWhileApprovalWaitsWins(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	l := NewMemLedger()
	a := pinAgent(t, pinConfig(t, "pintest/base"), l, rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {}})
	rec.script(callTools(use("d1", "delta", `{}`)), endTurn("done"))
	r, err := a.Run(ctx, "change something", AsAgent("worker"))
	mustState(t, r, err, StateInputRequired)
	setProfiles(t, a, map[string]AgentProfile{"worker": {Policy: &Rules{Deny: []string{"delta"}}}})
	if err := a.Decide(ctx, r.ID, ApprovalDecision{ApprovalID: r.Pending.ID, Decision: "allow"}); err != nil {
		t.Fatal(err)
	}
	r, err = a.Wait(ctx, r.ID)
	mustState(t, r, err, StateCompleted)
	deniedByPolicy(t, l, r.ID, "d1")
}

// A branch of a branch keeps the text of the first source in this process,
// although the branch between them made no model call: a branch without a
// message of a finished run, and a branch that waits for an approval.
func TestBranchOfBranchKeepsTheText(t *testing.T) {
	ctx := testCtx(t)
	rec := &pinRecorder{}
	l := NewMemLedger()
	a := pinAgent(t, pinConfig(t, "pintest/base"), l, rec)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "Old."}})
	rec.script(endTurn("one"), callTools(use("d1", "delta", `{}`)))
	s, err := a.Run(ctx, "first", AsAgent("worker"))
	mustState(t, s, err, StateCompleted)
	w, err := a.Run(ctx, "change something", AsAgent("worker"))
	mustState(t, w, err, StateInputRequired)
	setProfiles(t, a, map[string]AgentProfile{"worker": {System: "New."}})

	finished, err := a.Fork(ctx, s.ID, ForkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := a.Fork(ctx, w.ID, ForkRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Get(ctx, waiting.ID); err != nil || got.State != StateInputRequired {
		t.Fatalf("branch of the waiting run = %+v, %v", got, err)
	}
	for i, mid := range []string{finished.ID, waiting.ID} {
		rec.script(endTurn("two"))
		b, err := a.Fork(ctx, mid, ForkRequest{Input: "go on"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.Wait(ctx, b.ID)
		if err == nil && got.State == StateInputRequired {
			// The branch of the waiting run copied its approval.
			if err := a.Decide(ctx, b.ID, ApprovalDecision{ApprovalID: got.Pending.ID, Decision: "allow"}); err != nil {
				t.Fatal(err)
			}
			got, err = a.Wait(ctx, b.ID)
		}
		mustState(t, got, err, StateCompleted)
		rec.expectAt(t, 2+i, "base", "Old.", "alpha,beta,gamma,delta")
		if n := driftNotes(t, l, b.ID); len(n) != 0 {
			t.Fatalf("branch %d records a change: %v", i, n)
		}
	}
}

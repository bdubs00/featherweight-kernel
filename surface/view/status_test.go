package view

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func TestComputeStatus(t *testing.T) {
	t0 := time.Date(2026, 10, 13, 9, 14, 31, 0, time.UTC)
	usage := func(in, out int) fw.RunUsage {
		return fw.RunUsage{Usage: fw.Usage{InputTokens: in, OutputTokens: out, CacheReadTokens: 999}}
	}
	runs := []*fw.Run{
		{ID: "r1", State: fw.StateCompleted, Usage: usage(1000, 100)},
		{ID: "r2", State: fw.StateInputRequired, Usage: usage(200, 20), Pending: &fw.Approval{
			ID: "ap_new", Tool: "files", Input: json.RawMessage(`{"path":"a.txt","op":"delete"}`), RequestedAt: t0.Add(time.Minute),
		}},
		nil,
		{ID: "r3", State: fw.StateInputRequired, Usage: usage(30, 3), Pending: &fw.Approval{
			ID: "ap_7f3", Tool: "bash", Input: json.RawMessage(`{"command":"git push\n  origin main"}`), RequestedAt: t0.In(time.FixedZone("x", 3600)),
		}},
		{ID: "r4", State: fw.StateRunning, Usage: usage(4, 1)},
		{ID: "r5", State: fw.StateFailed},
		{ID: "r6", State: fw.StateSuspended},
	}
	got := ComputeStatus(runs)
	want := Status{
		Usage:           StatusUsage{PromptTokens: 1234, CompletionTokens: 124},
		PendingApproval: &PendingApproval{ID: "ap_7f3", Action: "bash: git push origin main", RequestedAt: t0},
		Runs:            RunCounts{Active: 4, InputRequired: 2, Total: 6},
	}
	if got.Usage != want.Usage || got.Runs != want.Runs {
		t.Errorf("status = %+v, want %+v", got, want)
	}
	if got.PendingApproval == nil || *got.PendingApproval != *want.PendingApproval {
		t.Errorf("pendingApproval = %+v, want %+v", got.PendingApproval, want.PendingApproval)
	}
	// r4 runs, so the process is not idle, and no idle time is given.
	if got.Idle || got.IdleSince != nil {
		t.Errorf("idle = %v since %v with a running run", got.Idle, got.IdleSince)
	}
	// Without a queued or running run the process is idle since the newest
	// update of a run, whatever else waits.
	idle := ComputeStatus([]*fw.Run{
		{ID: "a", State: fw.StateCompleted, UpdatedAt: t0},
		{ID: "b", State: fw.StateInputRequired, UpdatedAt: t0.Add(2 * time.Minute)},
		{ID: "c", State: fw.StateSuspended, UpdatedAt: t0.Add(time.Minute)},
	})
	if !idle.Idle || idle.IdleSince == nil || !idle.IdleSince.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("idle status = %+v (since %v)", idle, idle.IdleSince)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"requestedAt":"2026-10-13T09:14:31Z"`, `"promptTokens":1234`, `"inputRequired":2`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON lacks %s: %s", key, b)
		}
	}
	// No runs give zero counts and a null approval, not a missing key.
	b, _ = json.Marshal(ComputeStatus(nil))
	if string(b) != `{"usage":{"promptTokens":0,"completionTokens":0},"pendingApproval":null,"runs":{"active":0,"inputRequired":0,"total":0},"idle":true}` {
		t.Errorf("empty status = %s", b)
	}
}

func TestApprovalAction(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		ap   fw.Approval
		want string
	}{
		{fw.Approval{Tool: "bash", Input: json.RawMessage(`{"command":"rm -rf build"}`)}, "bash: rm -rf build"},
		{fw.Approval{Tool: "files", Input: json.RawMessage(`{"path":"a"}`)}, `files: {"path":"a"}`},
		{fw.Approval{Tool: "git.commit", Input: json.RawMessage(`{}`), Reason: "rule: ask git.*"}, "git.commit: rule: ask git.*"},
		{fw.Approval{Reason: "Which branch?"}, "input: Which branch?"},
		{fw.Approval{Tool: "deploy"}, "deploy"},
	}
	for _, tt := range tests {
		if got := ApprovalAction(&tt.ap); got != tt.want {
			t.Errorf("ApprovalAction(%+v) = %q, want %q", tt.ap, got, tt.want)
		}
	}
	got := ApprovalAction(&fw.Approval{Tool: "bash", Input: json.RawMessage(`{"command":"` + long + `"}`)})
	if n := len([]rune(got)); n != len("bash: ")+maxActionSummary {
		t.Errorf("long action has %d runes: %q", n, got)
	}
}

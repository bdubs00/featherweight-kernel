package view

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Status is the body of GET /v1/status: the state of the agent at a glance,
// for a control plane or a poller. Under ax, the workbench reads it through
// the atenet router (docs/ARCHITECTURE.md section 11.8). It counts the runs
// that the caller can view.
type Status struct {
	Usage           StatusUsage      `json:"usage"`
	PendingApproval *PendingApproval `json:"pendingApproval"`
	Runs            RunCounts        `json:"runs"`
	// Idle is true when no run works: every run waits for input, is
	// suspended, or is finished. A process without runs is idle. A
	// controller suspends a Task that stays idle (docs/ARCHITECTURE.md
	// section 11.8).
	Idle bool `json:"idle"`
	// IdleSince is when the last run stopped working, or when the process
	// started if no run worked yet. It is absent while a run works.
	IdleSince *time.Time `json:"idleSince,omitempty"`
}

// StatusUsage is the token usage over every run.
type StatusUsage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
}

// PendingApproval is the oldest approval that waits for a decision.
type PendingApproval struct {
	ID          string    `json:"id"`
	Action      string    `json:"action"`
	RequestedAt time.Time `json:"requestedAt"`
}

// RunCounts counts the runs by state.
type RunCounts struct {
	// Active counts the runs that are not in a terminal state.
	Active        int `json:"active"`
	InputRequired int `json:"inputRequired"`
	Total         int `json:"total"`
}

// StatusOf computes the status from the runs that the caller in ctx can
// view. A kernel that does not list runs gives zero counts. An idle process
// whose runs never worked is idle since its start. A process that is
// sending a branch to another process (fw.Agent.Starting) is not idle.
func StatusOf(ctx context.Context, a *fw.Agent) (Status, error) {
	runs, err := a.List(ctx)
	if err != nil && !errors.Is(err, fw.ErrNotImplemented) {
		return Status{}, err
	}
	s := ComputeStatus(runs)
	if a.Starting() > 0 {
		s.Idle, s.IdleSince = false, nil
	}
	if s.Idle && s.IdleSince == nil {
		t := a.Started().UTC()
		s.IdleSince = &t
	}
	return s, nil
}

// ComputeStatus builds the status from a list of runs. The runs are idle
// when none is queued or running; IdleSince is then the newest update of
// a run, or absent when the list is empty.
func ComputeStatus(runs []*fw.Run) Status {
	s := Status{Idle: true}
	var last time.Time
	for _, r := range runs {
		if r == nil {
			continue
		}
		if r.State == fw.StateQueued || r.State == fw.StateRunning {
			s.Idle = false
		}
		if r.UpdatedAt.After(last) {
			last = r.UpdatedAt
		}
		s.Runs.Total++
		s.Usage.PromptTokens += r.Usage.InputTokens
		s.Usage.CompletionTokens += r.Usage.OutputTokens
		if !r.State.Terminal() {
			s.Runs.Active++
		}
		if r.State == fw.StateInputRequired {
			s.Runs.InputRequired++
		}
		if p := r.Pending; p != nil {
			if s.PendingApproval == nil || p.RequestedAt.Before(s.PendingApproval.RequestedAt) {
				s.PendingApproval = &PendingApproval{ID: p.ID, Action: ApprovalAction(p), RequestedAt: p.RequestedAt.UTC()}
			}
		}
	}
	if s.Idle && !last.IsZero() {
		t := last.UTC()
		s.IdleSince = &t
	}
	return s
}

const maxActionSummary = 120

// ApprovalAction describes an approval as "<tool>: <command or input
// summary>". The summary is the "command" field of the input when there is
// one, else the input as compact JSON, cut to 120 characters.
func ApprovalAction(p *fw.Approval) string {
	tool := p.Tool
	if tool == "" {
		tool = "input"
	}
	summary := approvalInputSummary(p.Input)
	if summary == "" {
		summary = p.Reason
	}
	summary = truncateRunes(strings.Join(strings.Fields(summary), " "), maxActionSummary)
	if summary == "" {
		return tool
	}
	return tool + ": " + summary
}

func approvalInputSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &in); err == nil && in.Command != "" {
		return in.Command
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

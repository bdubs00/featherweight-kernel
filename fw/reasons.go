package fw

// This file holds the typed reasons of a wait and of a stop
// (docs/ARCHITECTURE.md section 25). fw.approval.requested carries wait:
// what the run waits for from a person. fw.run.finished and
// fw.run.suspended carry stop: why the run stopped. Both ride on events
// that the egress stream of a fleet carries already, so a flow view counts
// the waits and the stops by kind with no parser and no new event type.
// An event of an older kernel has neither field. WaitOf and StopOf derive
// the reason of such an event from its other fields, so a consumer reads
// old and new ledgers the same way.

import "strings"

// The values of ApprovalRequestedData.Wait.
const (
	// WaitApproval: the policy asks a person before a tool call runs.
	WaitApproval = "approval"
	// WaitQuestion: a tool asks a person a question with a form.
	WaitQuestion = "question"
	// WaitConnect: a tool call waits for a person to connect a service.
	WaitConnect = "connect"
	// WaitRerun: a restart cut a call off that can already have changed
	// something, and a person decides whether it runs again.
	WaitRerun = "rerun"
)

// reasonRerun is the reason of the approval of a call that a restart cut
// off (WaitRerun).
const reasonRerun = "rerun after resume"

// The values of RunFinishedData.Stop and RunSuspendedData.Stop. A stop at
// a limit of the budget is RunStopBudget of the limit, such as
// budget:maxTurns.
const (
	// RunStopAnswered: the model ended its turn with an answer, or a
	// routine or a check run gave the output.
	RunStopAnswered = "answered"
	// RunStopChecked: the model answered, and every check of the
	// definition of done passed (section 24).
	RunStopChecked = "checked"
	// RunStopNotDone: the checks of the definition of done failed at the
	// last attempt.
	RunStopNotDone = "not-done"
	// RunStopLoop: the model repeated the same tool call, or the same
	// cycle of calls, up to budget.maxRepeats.
	RunStopLoop = "loop"
	// RunStopRefusal: the model refused the task.
	RunStopRefusal = "refusal"
	// RunStopMaxTokens: the model stopped at its output limit again after
	// a continuation.
	RunStopMaxTokens = "max-tokens"
	// RunStopModelError: the call of the model failed, after the retries
	// of its adapter.
	RunStopModelError = "model-error"
	// RunStopCanceled: a person or a process canceled the run.
	RunStopCanceled = "canceled"
	// RunStopInterrupted: the process stopped while the run worked: a
	// shutdown suspended it, or a restart cut off a call that cannot run
	// twice.
	RunStopInterrupted = "interrupted"
	// RunStopError: the kernel failed the run for another reason, such as
	// a check run whose step failed or a driver that stopped on an error.
	RunStopError = "error"
)

// runStopBudgetPrefix starts the stop of a run at a limit of its budget.
const runStopBudgetPrefix = "budget:"

// RunStopBudget returns the stop of a run at a limit of its budget, such
// as budget:maxTurns. The limit maxRepeats is a loop (RunStopLoop).
func RunStopBudget(limit string) string {
	if limit == "maxRepeats" {
		return RunStopLoop
	}
	return runStopBudgetPrefix + limit
}

// StopLimit returns the limit of a stop at the budget, such as maxTurns
// for budget:maxTurns, and "" for every other stop.
func StopLimit(stop string) string {
	limit, ok := strings.CutPrefix(stop, runStopBudgetPrefix)
	if !ok {
		return ""
	}
	return limit
}

// WaitOf returns the wait of an approval request: its field Wait, or for
// an event of an older kernel, the wait that its other fields show.
func WaitOf(d ApprovalRequestedData) string {
	switch {
	case d.Wait != "":
		return d.Wait
	case d.Connect != nil:
		return WaitConnect
	case len(d.Schema) > 0:
		return WaitQuestion
	case d.Reason == reasonRerun:
		return WaitRerun
	}
	return WaitApproval
}

// StopOf returns the stop of the end of a run: its field Stop, or for an
// event of an older kernel, the stop that its outcome, its definition of
// done, and the start of its error show.
func StopOf(d RunFinishedData) string {
	if d.Stop != "" {
		return d.Stop
	}
	switch d.Outcome {
	case OutcomeCompleted:
		if d.Done == DoneChecked {
			return RunStopChecked
		}
		return RunStopAnswered
	case OutcomeCanceled:
		return RunStopCanceled
	}
	msg := d.Error
	switch {
	case d.Done == DoneFailed || strings.HasPrefix(msg, "not done:"):
		return RunStopNotDone
	case strings.HasPrefix(msg, "budget exceeded: "):
		limit, _, _ := strings.Cut(strings.TrimPrefix(msg, "budget exceeded: "), " ")
		limit = strings.TrimRight(limit, ":;,.")
		if limit == "" {
			return RunStopError
		}
		return RunStopBudget(limit)
	case strings.HasPrefix(msg, "model:"):
		return RunStopModelError
	case msg == "max_tokens":
		return RunStopMaxTokens
	case msg == checkInterrupted:
		return RunStopInterrupted
	}
	return RunStopError
}

// suspendStopOf returns the stop of a suspend: its field Stop, else
// RunStopInterrupted, the only cause of a suspend today.
func suspendStopOf(d RunSuspendedData) string {
	if d.Stop != "" {
		return d.Stop
	}
	return RunStopInterrupted
}

// withReasons fills the wait of an approval request and the stop of the
// end and of the suspend of a run that a site of the kernel or a linked
// runner left empty, so every new event carries its reason. Every other
// data passes unchanged.
func withReasons(data any) any {
	switch d := data.(type) {
	case ApprovalRequestedData:
		d.Wait = WaitOf(d)
		return d
	case RunFinishedData:
		d.Stop = StopOf(d)
		return d
	case RunSuspendedData:
		d.Stop = suspendStopOf(d)
		return d
	}
	return data
}

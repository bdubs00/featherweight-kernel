package fw

// This file is the check run (docs/ARCHITECTURE.md section 23): a run that
// makes the tool calls of its request in order, under the policy of its
// agent, and finishes with their results and no model call. The front of
// a fleet starts one to look at a queue or a service before it wakes the
// model of a duty, so a quiet queue costs no tokens. The calls go through
// the tool path of the routine runner (routine_run.go), so the budget, the
// policy, the limits, and the ledger treat them as calls of the model.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// MetaKind is the metadata key of the kind of a run. Only the kernel sets
// it: RunKindCheck on a check run. An ordinary run has none.
const MetaKind = "fw.kind"

// RunKindCheck is the kind of a check run (MetaKind).
const RunKindCheck = "check"

// Kind returns the kind of the run, the metadata value MetaKind:
// RunKindCheck for a check run, and "" for an ordinary run.
func (r *Run) Kind() string { return r.Metadata[MetaKind] }

// Bounds and defaults of a check.
const (
	// MaxCheckSteps is the most steps of a check.
	MaxCheckSteps = 5
	// DefaultCheckTimeout is the time limit of a check that sets none.
	DefaultCheckTimeout = 30 * time.Second
	// MaxCheckTimeout is the longest time limit of a check.
	MaxCheckTimeout = 60 * time.Second
	// DefaultCheckMaxBytes is the output limit of a step of a check that
	// sets none: 1 MiB.
	DefaultCheckMaxBytes = 1 << 20
	// MaxCheckMaxBytes is the largest output limit of a step: 4 MiB.
	MaxCheckMaxBytes = 4 << 20
)

// checkCallPrefix starts the call id of every call of a check.
const checkCallPrefix = "check_"

// checkInterrupted is the error of a check run that a restart or a
// shutdown cut off after its first call. The kernel never runs a check
// twice, because it cannot know that its tools only read.
const checkInterrupted = "the process stopped during the check"

// errCheckTimeout is the cause of the end of a call at the time limit of
// its check.
var errCheckTimeout = errors.New("fw: the check reached its time limit")

// Check is the check of a run request (RunRequest.Check): the tool calls
// of a run that calls no model. The kernel makes the calls of Steps in
// order, with the policy of the agent, and the run finishes with their
// results as its output (CheckOutput). A step whose tool the policy asks
// for or denies fails the run, because nobody answers a check.
//
// Timeout bounds the whole check: zero means DefaultCheckTimeout, and the
// limit is MaxCheckTimeout. MaxBytes bounds the output of each step: zero
// means DefaultCheckMaxBytes, and the limit is MaxCheckMaxBytes. In JSON,
// timeout is a duration string such as "30s", or a number of nanoseconds.
type Check struct {
	Steps    []CheckStep   `json:"steps"`
	Timeout  time.Duration `json:"timeout,omitempty"`
	MaxBytes int           `json:"maxBytes,omitempty"`
}

// CheckStep is one tool call of a check. Input is a JSON object; empty
// means {}.
type CheckStep struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// checkJSON is the JSON form of Check, with the timeout as a duration
// string.
type checkJSON struct {
	Steps    []CheckStep     `json:"steps"`
	Timeout  json.RawMessage `json:"timeout,omitempty"`
	MaxBytes int             `json:"maxBytes,omitempty"`
}

// MarshalJSON writes the timeout as a duration string, such as "30s".
func (c Check) MarshalJSON() ([]byte, error) {
	out := checkJSON{Steps: c.Steps, MaxBytes: c.MaxBytes}
	if c.Timeout != 0 {
		out.Timeout, _ = json.Marshal(c.Timeout.String())
	}
	return json.Marshal(out)
}

// UnmarshalJSON takes the timeout as a duration string, such as "30s", or
// as a number of nanoseconds.
func (c *Check) UnmarshalJSON(b []byte) error {
	var in checkJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*c = Check{Steps: in.Steps, MaxBytes: in.MaxBytes}
	raw := bytes.TrimSpace(in.Timeout)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("fw: check.timeout %q is not a duration such as \"30s\"", s)
		}
		c.Timeout = d
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("fw: check.timeout is a duration string such as \"30s\" or a number of nanoseconds")
	}
	c.Timeout = time.Duration(n)
	return nil
}

// Resolved returns the check with the defaults of its timeout and its
// output limit, and the input of each step as a compact JSON object, {}
// for an empty one. An error with ErrInvalidRequest names the first
// problem: 1 to MaxCheckSteps steps, a tool name in each, an input that
// is a JSON object, a timeout from 0 to MaxCheckTimeout, and an output
// limit from 0 to MaxCheckMaxBytes.
func (c Check) Resolved() (Check, error) {
	bad := func(format string, args ...any) (Check, error) {
		return Check{}, fmt.Errorf("%w: "+format, append([]any{ErrInvalidRequest}, args...)...)
	}
	if len(c.Steps) == 0 || len(c.Steps) > MaxCheckSteps {
		return bad("check.steps has %d steps; a check has 1 to %d", len(c.Steps), MaxCheckSteps)
	}
	out := Check{Steps: make([]CheckStep, len(c.Steps)), Timeout: c.Timeout, MaxBytes: c.MaxBytes}
	for i, s := range c.Steps {
		if strings.TrimSpace(s.Tool) == "" {
			return bad("check.steps[%d].tool is empty", i)
		}
		in := bytes.TrimSpace(s.Input)
		if len(in) == 0 || bytes.Equal(in, []byte("null")) {
			in = []byte("{}")
		}
		var obj map[string]json.RawMessage
		var b bytes.Buffer
		if json.Unmarshal(in, &obj) != nil || obj == nil || json.Compact(&b, in) != nil {
			return bad("check.steps[%d].input is not a JSON object", i)
		}
		out.Steps[i] = CheckStep{Tool: s.Tool, Input: b.Bytes()}
	}
	switch {
	case c.Timeout < 0:
		return bad("check.timeout %s is negative", c.Timeout)
	case c.Timeout > MaxCheckTimeout:
		return bad("check.timeout %s is longer than the limit of %s", c.Timeout, MaxCheckTimeout)
	case c.Timeout == 0:
		out.Timeout = DefaultCheckTimeout
	}
	switch {
	case c.MaxBytes < 0:
		return bad("check.maxBytes %d is negative", c.MaxBytes)
	case c.MaxBytes > MaxCheckMaxBytes:
		return bad("check.maxBytes %d is above the limit of %d (4 MiB)", c.MaxBytes, MaxCheckMaxBytes)
	case c.MaxBytes == 0:
		out.MaxBytes = DefaultCheckMaxBytes
	}
	return out, nil
}

// CheckOutput is the output of a check run, as JSON: one result per step,
// in the order of the steps. A check run that failed holds the results of
// the steps before the failure, or no output when none ran.
type CheckOutput struct {
	Results []CheckResult `json:"results"`
}

// CheckResult is the result of one step of a check. Output is the text of
// the result of the tool as JSON when it parses as JSON, the structured
// content of the result when its text does not, and else the text as a
// JSON string. IsError is true for an error result of the tool, which does
// not fail the run. Cut is true when the result was longer than the output
// limit of the check: Output then holds the first bytes of the text, as a
// JSON string. A result of a tool that holds an image or a document also
// counts as cut, because the output keeps only its text.
type CheckResult struct {
	Tool    string          `json:"tool"`
	Output  json.RawMessage `json:"output"`
	IsError bool            `json:"isError"`
	Cut     bool            `json:"cut,omitempty"`
}

// stepCheck runs the check of a check run and finishes the run: completed
// with the results, or failed with the reason. It acts once. A check that
// a restart or a shutdown cut off after its first call fails, because a
// call can have changed something.
func (a *Agent) stepCheck(ctx context.Context, rs *runState, c *cursor) error {
	if c.toolCalls > c.keptCalls {
		return a.fail(ctx, rs, checkInterrupted)
	}
	// The start resolved the check. A ledger that another process wrote
	// gets the defaults here too.
	chk, err := c.queued.Check.Resolved()
	if err != nil {
		return a.fail(ctx, rs, strings.TrimPrefix(err.Error(), ErrInvalidRequest.Error()+": "))
	}
	prof := a.profileOf(c.queued)
	_, tools := a.runTools(ctx, rs, prof)
	rr := &routineRun{a: a, rs: rs, prof: prof, tools: tools, policy: a.policyFor(rs, prof), prefix: checkCallPrefix, check: true}
	deadline := time.Now().Add(chk.Timeout)
	out := CheckOutput{Results: []CheckResult{}}
	for i, s := range chk.Steps {
		label := fmt.Sprintf("check step %d (%s)", i+1, s.Tool)
		if !time.Now().Before(deadline) {
			return a.failCheck(ctx, rs, out, fmt.Sprintf("%s: the check did not finish within %s", label, chk.Timeout))
		}
		t, ok := tools[s.Tool]
		if !ok {
			return a.failCheck(ctx, rs, out, label+": the agent has no tool of this name")
		}
		ct := &checkTool{Tool: t, deadline: deadline, max: chk.MaxBytes}
		res, why, err := rr.call(ctx, "", ct, s.Input)
		switch {
		case err != nil:
			return err
		case why != "":
			return a.failCheck(ctx, rs, out, label+": "+why)
		case ct.late:
			return a.failCheck(ctx, rs, out, fmt.Sprintf("%s: the check did not finish within %s", label, chk.Timeout))
		}
		out.Results = append(out.Results, checkResultOf(s.Tool, res, ct.cut))
	}
	text, err := checkText(out)
	if err != nil {
		return a.failCheck(ctx, rs, CheckOutput{}, "the output of the check: "+err.Error())
	}
	return a.finish(ctx, rs, OutcomeCompleted, text, "")
}

// failCheck records the reason of a check that failed and finishes the
// run as failed, with the results of the steps that ran.
func (a *Agent) failCheck(ctx context.Context, rs *runState, out CheckOutput, why string) error {
	if _, err := a.emit(ctx, rs, EvError, ErrorData{Message: why}); err != nil {
		return err
	}
	text := ""
	if len(out.Results) > 0 {
		text, _ = checkText(out)
	}
	return a.finish(ctx, rs, OutcomeFailed, text, why)
}

// checkText returns the output of a check as JSON text.
func checkText(out CheckOutput) (string, error) {
	b, err := plainJSON(out)
	return string(b), err
}

// plainJSON is json.Marshal without the HTML escapes, so a check keeps
// the text of a tool as it was.
func plainJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// checkResultOf returns the result of one step from the result of its
// tool, as the ledger recorded it.
func checkResultOf(tool string, res ToolResult, cut bool) CheckResult {
	r := CheckResult{Tool: tool, IsError: res.IsError, Cut: cut}
	text := TextOf(res.Content)
	var b bytes.Buffer
	if t := bytes.TrimSpace([]byte(text)); !cut && len(t) > 0 && json.Valid(t) && json.Compact(&b, t) == nil {
		r.Output = b.Bytes()
		return r
	}
	r.Output, _ = plainJSON(text)
	return r
}

// checkTool is the tool of one step of a check. Its call ends at the
// deadline of the check, and it asks nobody: a question or a connect of
// the tool gets ErrNoAsker, because nobody answers a check. The output
// limit of the tools (tools.limits.maxOutputBytes) does not cut its
// result, because no model reads it. The check keeps at most max bytes of
// the text instead, before the ledger records the result.
type checkTool struct {
	Tool
	deadline time.Time
	max      int
	cut      bool // the result was longer than max, or held more than text
	late     bool // the call ended at the deadline of the check
}

// Unwrap returns the tool of the step, so the policy resolves the call of
// the tool (ResolveCall).
func (t *checkTool) Unwrap() Tool { return t.Tool }

// Call implements Tool.
func (t *checkTool) Call(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	ctx, cancel := context.WithDeadlineCause(ctx, t.deadline, errCheckTimeout)
	defer cancel()
	ctx = WithConnecter(WithAsker(withWholeOutput(ctx), nil), nil)
	res, err := t.Tool.Call(ctx, input)
	if context.Cause(ctx) == errCheckTimeout {
		t.late = true
		return ErrorResult(fmt.Sprintf("%s: the check reached its time limit, and the call stopped", t.Spec().Name)), nil
	}
	if err != nil {
		return res, err
	}
	res, t.cut = cutCheck(res, t.max)
	return res, nil
}

// cutCheck keeps the text of a result, at most n bytes of it, and reports
// whether the result lost anything: text past n bytes, or a block that is
// not text. A result with structured content becomes the text of that
// content, as the ledger records it, when it has no text or when its text
// is no JSON, as a tool that writes words for a model and JSON for a
// program does. A check reads JSON, so it reads the structured content of
// such a tool.
func cutCheck(res ToolResult, n int) (ToolResult, bool) {
	if s := bytes.TrimSpace(res.Structured); len(s) > 0 && json.Valid(s) &&
		(len(res.Content) == 0 || !json.Valid(bytes.TrimSpace([]byte(TextOf(res.Content))))) {
		res.Content = []Block{Text(string(s))}
	}
	res.Structured = nil
	text := TextOf(res.Content)
	other := slices.ContainsFunc(res.Content, func(b Block) bool { return b.Type != BlockText })
	if len(text) <= n && !other {
		return res, false
	}
	if len(text) > n {
		for n > 0 && !utf8.RuneStart(text[n]) {
			n--
		}
		text = text[:n]
	}
	res.Content = []Block{Text(text)}
	return res, true
}

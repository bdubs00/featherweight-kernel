package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// deciderCLI names the command line as the decider of an approval.
const deciderCLI = "cli"

// runArgs holds the arguments of fw run.
type runArgs struct {
	input     string
	agent     string
	json      bool
	noApprove bool

	// The profile flags. set holds the name of every flag on the command
	// line, so an empty --skills means no skill and an absent one keeps the
	// skills of the profile.
	system       string
	instructions multiFlag
	tools        string
	skills       string
	model        string
	effort       string
	timeZone     string
	maxTurns     int
	deadline     time.Duration
	maxToolCalls int
	set          map[string]bool
}

// multiFlag collects the values of a flag that repeats.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ", ") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// parseRunArgs parses `run "<input>"` with its flags. The words of the
// input are joined by spaces, and "-" reads the input from standard input.
// It prints every error it returns.
func (g *globals) parseRunArgs(args []string) (runArgs, error) {
	r := runArgs{set: map[string]bool{}}
	fs := g.newFlagSet("run", `"<input>"`)
	fs.StringVar(&r.agent, "agent", os.Getenv("FW_AGENT"), "the agent profile `name` (default: $FW_AGENT, else default)")
	fs.BoolVar(&r.json, "json", false, "print the run as JSON instead of the output")
	fs.BoolVar(&r.noApprove, "no-approve", false, "deny every approval and question without asking")
	fs.StringVar(&r.system, "system", "", "the system prompt `text`, or @path to read it from a file; it replaces the prompt of the profile")
	fs.Var(&r.instructions, "instructions", "`text` that follows the system prompt, or @path to read a file; repeat the flag for more")
	fs.Var(&r.instructions, "i", "short for -instructions")
	fs.StringVar(&r.tools, "tools", "", "comma-separated `globs` over the tool names the run can call, for example files,git.*")
	fs.StringVar(&r.skills, "skills", "", "comma-separated `globs` over the skill names the run can use; none allows no skill")
	fs.StringVar(&r.model, "model", "", "the model `selector`, such as anthropic/claude-haiku-5-5, or a name from models")
	fs.StringVar(&r.effort, "effort", "", "the reasoning `effort`: "+strings.Join(fw.Efforts, ", "))
	fs.StringVar(&r.timeZone, "time-zone", "", "the IANA time `zone` of the run, for example America/Chicago")
	fs.IntVar(&r.maxTurns, "max-turns", 0, "the most model turns (`count`)")
	fs.DurationVar(&r.deadline, "deadline", 0, "the most working time of the run (`duration`, for example 30m); time spent waiting does not count")
	fs.IntVar(&r.maxToolCalls, "max-tool-calls", 0, "the most tool calls in the run (`count`)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return r, err // the flag package printed it
	}
	fs.Visit(func(f *flag.Flag) { r.set[f.Name] = true })
	if err := r.checkProfileFlags(); err != nil {
		fmt.Fprintf(g.stderr, "fw run: %v\n", err)
		return r, err
	}
	r.input = strings.Join(pos, " ")
	if r.input == "-" {
		b, err := io.ReadAll(g.stdin)
		if err != nil {
			fmt.Fprintf(g.stderr, "fw run: read the input: %v\n", err)
			return r, err
		}
		r.input = string(b)
	}
	if strings.TrimSpace(r.input) == "" {
		fmt.Fprintln(g.stderr, "fw run: the input is empty")
		fs.Usage()
		return r, errors.New("fw run: the input is empty")
	}
	return r, nil
}

// checkProfileFlags reads the files behind @path values and checks the
// values that the kernel would refuse, so the error names the flag.
func (r *runArgs) checkProfileFlags() error {
	var err error
	if r.system, err = textOrFile(r.system); err != nil {
		return fmt.Errorf("-system: %w", err)
	}
	for i, v := range r.instructions {
		if r.instructions[i], err = textOrFile(v); err != nil {
			return fmt.Errorf("-instructions: %w", err)
		}
	}
	if !fw.ValidEffort(r.effort) {
		return fmt.Errorf("-effort %q is not one of %s", r.effort, strings.Join(fw.Efforts, ", "))
	}
	if r.timeZone != "" {
		if _, _, err := fw.LoadZone(r.timeZone); err != nil {
			return fmt.Errorf("-time-zone %q is not an IANA time zone such as America/Chicago, or UTC", r.timeZone)
		}
	}
	if r.maxTurns < 0 || r.deadline < 0 || r.maxToolCalls < 0 {
		return errors.New("a limit flag is negative")
	}
	return nil
}

// textOrFile returns v, or the contents of the file that v names after a
// leading "@".
func textOrFile(v string) (string, error) {
	path, ok := strings.CutPrefix(v, "@")
	if !ok {
		return v, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// profile builds the inline profile of the run from the flags that are
// set, or returns nil when none is.
func (r *runArgs) profile() *fw.AgentProfile {
	p := fw.AgentProfile{
		System:       r.system,
		Instructions: r.instructions,
		Model:        r.model,
		Effort:       strings.ToLower(r.effort),
		TimeZone:     r.timeZone,
	}
	some := p.System != "" || len(p.Instructions) > 0 || p.Model != "" || p.Effort != "" || p.TimeZone != ""
	if r.set["tools"] {
		p.Tools, some = splitGlobs(r.tools), true
	}
	if r.set["skills"] {
		p.Skills, some = splitGlobs(r.skills), true
	}
	if r.maxTurns > 0 || r.deadline > 0 || r.maxToolCalls > 0 {
		p.Budget = &fw.Budget{MaxTurns: r.maxTurns, Deadline: r.deadline, MaxToolCalls: r.maxToolCalls}
		some = true
	}
	if !some {
		return nil
	}
	return &p
}

// splitGlobs splits a comma-separated list. The word none, or an empty
// value, is an empty list, which allows nothing.
func splitGlobs(v string) []string {
	out := []string{}
	for _, g := range strings.Split(v, ",") {
		if g = strings.TrimSpace(g); g != "" && g != "none" {
			out = append(out, g)
		}
	}
	return out
}

func cmdRun(ctx context.Context, g *globals, args []string) int {
	ra, err := g.parseRunArgs(args)
	if err != nil {
		return flagExit(err)
	}
	cfg, err := g.loadConfig()
	if err != nil {
		return g.fail("run", err)
	}
	a, err := newAgent(ctx, cfg, modeOnce)
	if err != nil {
		return g.fail("run", err)
	}
	defer closeAgent(a)
	ctx = commandContext(ctx, cfg)

	opts := []fw.RunOption{fw.AsAgent(ra.agent), fw.WithOrigin(fw.OriginCLI)}
	if p := ra.profile(); p != nil {
		opts = append(opts, fw.WithRunProfile(*p))
	}
	r, err := a.Start(ctx, ra.input, opts...)
	if err != nil {
		return g.fail("run", accessHint(cfg, err))
	}
	if len(r.ExtraTools) > 0 {
		fmt.Fprintf(g.stderr, "fw run: for this run only, -tools gives %s tools that its profile does not list: %s\n", r.Agent, strings.Join(r.ExtraTools, ", "))
	}
	var ap approver
	switch {
	case ra.noApprove:
		ap = g.denyApprover("--no-approve is set")
	case isTerminal(g.stdin):
		ap = g.terminalApprover()
	default:
		ap = g.denyApprover("standard input is not a terminal")
	}
	final, err := followRun(ctx, a, r.ID, g.stderr, ap, true)
	if err != nil {
		return g.fail("run", err)
	}
	if final.State == fw.StateFailed {
		if limit := exceededLimit(a, final.ID); limit != "" {
			fmt.Fprintln(g.stderr, "fw run: "+budgetHint(limit))
		}
	}
	if err := g.printRun(final, ra.json); err != nil {
		return g.fail("run", err)
	}
	return runExit(final)
}

// exceededLimit returns the limit of the last fw.budget.exceeded event of
// a run, or "".
func exceededLimit(a *fw.Agent, id string) string {
	limit := ""
	for e, err := range a.Ledger().Read(context.Background(), id, 0) {
		if err != nil {
			break
		}
		if e.Type == fw.EvBudgetExceeded {
			var d fw.BudgetExceededData
			if e.Decode(&d) == nil {
				limit = d.Limit
			}
		}
	}
	return limit
}

// budgetFlags names the fw run flag that raises each limit, or "" when
// only the configuration sets it.
var budgetFlags = map[string]string{
	"maxTurns":        "-max-turns",
	"maxTokens":       "",
	"maxCostUSD":      "",
	"deadline":        "-deadline",
	"maxToolCalls":    "-max-tool-calls",
	"maxCallsPerTurn": "",
	"maxRepeats":      "",
}

// budgetHint is one line that names the limit a run stopped at and what
// raises it.
func budgetHint(limit string) string {
	key := "budget." + limit
	if flag, ok := budgetFlags[limit]; ok && flag != "" {
		return fmt.Sprintf("the run stopped at the limit %s; raise it with %s, or with %s in fw.yaml or in the profile", limit, flag, key)
	}
	if limit == "maxRepeats" {
		return fmt.Sprintf("the run stopped at the limit %s because the model repeated the same tool call; raise %s in fw.yaml or in the profile, or change the instructions", limit, key)
	}
	return fmt.Sprintf("the run stopped at the limit %s; raise it with %s in fw.yaml or in the profile", limit, key)
}

// printRun prints the output of a run, or the run as JSON.
func (g *globals) printRun(r *fw.Run, asJSON bool) error {
	if asJSON {
		b, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(g.stdout, "%s\n", b)
		return err
	}
	if r.Output == "" {
		return nil
	}
	out := r.Output
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err := io.WriteString(g.stdout, out)
	return err
}

// runExit maps the state of a finished run to the exit code.
func runExit(r *fw.Run) int {
	switch r.State {
	case fw.StateCompleted:
		return exitOK
	case fw.StateCanceled:
		return exitCanceled
	default:
		return exitFailed
	}
}

// approver answers one approval. It returns false when it leaves the
// approval to another surface.
type approver func(ctx context.Context, req fw.ApprovalRequestedData) (fw.ApprovalDecision, bool)

// followRun prints a one-line summary of every event of a run to w and
// answers each approval with ap, until the run ends. A nil ap leaves every
// approval to the surfaces. When ctx ends first, followRun cancels the run
// if cancelOnStop is set, and otherwise leaves it to the kernel, which
// suspends it at shutdown. It returns the final status.
func followRun(ctx context.Context, a *fw.Agent, id string, w io.Writer, ap approver, cancelOnStop bool) (*fw.Run, error) {
	evs, err := a.Events(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	tools := map[string]string{}
	for e := range evs {
		fmt.Fprintln(w, summarize(e, tools))
		if e.Type != fw.EvApprovalRequested || ap == nil {
			continue
		}
		var req fw.ApprovalRequestedData
		if err := e.Decode(&req); err != nil {
			continue
		}
		d, ok := ap(ctx, req)
		if !ok {
			continue
		}
		d.ApprovalID = req.ApprovalID
		d.Decider, d.DeciderKind = deciderCLI, fw.KindLocal
		if err := a.Decide(ctx, id, d); err != nil && !errors.Is(err, fw.ErrNoPendingApproval) && ctx.Err() == nil {
			return nil, err
		}
	}
	if ctx.Err() != nil && cancelOnStop {
		if err := a.Cancel(context.Background(), id); err != nil {
			return nil, err
		}
	}
	return a.Get(context.Background(), id)
}

// denyApprover denies every approval and declines every question.
func (g *globals) denyApprover(why string) approver {
	return func(_ context.Context, req fw.ApprovalRequestedData) (fw.ApprovalDecision, bool) {
		fmt.Fprintf(g.stderr, "fw run: denied %s for tool %s: %s\n", kindOf(req), req.Tool, why)
		return fw.ApprovalDecision{Decision: "deny", Note: "denied by fw run: " + why}, true
	}
}

// terminalApprover asks on the terminal: "allow? [y/N]" for an approval,
// and the answer for a question.
func (g *globals) terminalApprover() approver {
	in := bufio.NewReader(g.stdin)
	return func(ctx context.Context, req fw.ApprovalRequestedData) (fw.ApprovalDecision, bool) {
		if len(req.Schema) > 0 {
			return g.askQuestion(ctx, in, req), true
		}
		fmt.Fprintf(g.stderr, "\napproval %s: tool %s\n", req.ApprovalID, req.Tool)
		if len(req.Input) > 0 {
			fmt.Fprintf(g.stderr, "  input: %s\n", compactJSON(req.Input))
		}
		if req.Reason != "" {
			fmt.Fprintf(g.stderr, "  reason: %s\n", req.Reason)
		}
		fmt.Fprint(g.stderr, "allow? [y/N] ")
		line, err := readLine(ctx, in)
		if err != nil {
			return fw.ApprovalDecision{Decision: "deny", Note: "no answer on the terminal"}, true
		}
		switch strings.ToLower(line) {
		case "y", "yes":
			return fw.ApprovalDecision{Decision: "allow"}, true
		}
		return fw.ApprovalDecision{Decision: "deny", Note: "denied on the terminal"}, true
	}
}

// askQuestion asks a question from a tool. A free-text question takes one
// line. A form takes one line of JSON. An empty line declines.
func (g *globals) askQuestion(ctx context.Context, in *bufio.Reader, req fw.ApprovalRequestedData) fw.ApprovalDecision {
	free := isFreeText(req.Schema)
	fmt.Fprintf(g.stderr, "\nquestion from tool %s: %s\n", req.Tool, req.Reason)
	if !free {
		fmt.Fprintf(g.stderr, "  answer with one line of JSON for the schema %s\n", compactJSON(req.Schema))
	}
	fmt.Fprint(g.stderr, "answer (empty to decline): ")
	line, err := readLine(ctx, in)
	if err != nil || line == "" {
		return fw.ApprovalDecision{Decision: "deny", Note: "declined on the terminal"}
	}
	var content json.RawMessage
	if free {
		content, _ = json.Marshal(map[string]string{"answer": line})
	} else {
		if !json.Valid([]byte(line)) {
			fmt.Fprintln(g.stderr, "fw run: the answer is not JSON; declining")
			return fw.ApprovalDecision{Decision: "deny", Note: "the answer on the terminal was not JSON"}
		}
		content = json.RawMessage(line)
	}
	return fw.ApprovalDecision{Decision: "allow", Content: content}
}

// isFreeText reports whether a question schema asks for one text answer.
func isFreeText(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	_, ok := s.Properties["answer"]
	return ok && len(s.Properties) == 1
}

func kindOf(req fw.ApprovalRequestedData) string {
	if len(req.Schema) > 0 {
		return "question"
	}
	return "approval"
}

// readLine reads one line, or returns when ctx ends.
func readLine(ctx context.Context, in *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		s, err := in.ReadString('\n')
		if err == io.EOF && s != "" {
			err = nil
		}
		ch <- result{strings.TrimSpace(s), err}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// isTerminal reports whether r is a terminal. Tests replace it.
var isTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func compactJSON(raw json.RawMessage) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return string(raw)
	}
	return strings.TrimSpace(b.String())
}

// summarize returns one line for an event: the sequence number, the type,
// and the facts that matter for the type. tools maps call ids to tool
// names, so a result names its tool. The line of fw.run.queued names the
// work by its title before the agent. A summary never holds tool input or
// output, which can carry secrets.
func summarize(e fw.Event, tools map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%4d %s", e.Seq, e.Type)
	add := func(format string, args ...any) { fmt.Fprintf(&b, " "+format, args...) }
	switch e.Type {
	case fw.EvRunQueued:
		var d fw.RunQueuedData
		if e.Decode(&d) == nil {
			if title := queuedTitle(d); title != "" {
				add("title=%q", title)
			}
			add("agent=%s origin=%s", d.Agent, d.Origin)
		}
	case fw.EvRunStarted:
		var d fw.RunStartedData
		if e.Decode(&d) == nil {
			add("model=%s tools=%d", d.Model, len(d.Tools))
		}
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		if e.Decode(&d) == nil {
			add("role=%s origin=%s", d.Role, d.Origin)
			if d.From != nil {
				add("from=%s:%s", d.From.Kind, oneLine(d.From.Name))
			}
			if d.By != "" {
				add("by=%s", d.By)
			}
			if d.Pinned {
				add("pinned=true")
			}
		}
	case fw.EvTurnStarted, fw.EvTurnFinished:
		var d fw.TurnData
		if e.Decode(&d) == nil {
			add("turn=%d", d.Turn)
		}
	case fw.EvModelRequested:
		var d fw.ModelRequestedData
		if e.Decode(&d) == nil {
			add("model=%s", selector(d.Provider, d.Model))
		}
	case fw.EvModelResponded:
		var d fw.ModelRespondedData
		if e.Decode(&d) == nil {
			add("model=%s latency=%dms stop=%s in=%d out=%d", selector(d.Provider, d.Model), d.LatencyMS, d.StopReason, d.Usage.InputTokens, d.Usage.OutputTokens)
		}
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) == nil {
			tools[d.CallID] = d.Tool
			add("tool=%s", d.Tool)
		}
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		if e.Decode(&d) == nil {
			add("tool=%s latency=%dms", firstNonEmpty(tools[d.CallID], d.CallID), d.LatencyMS)
			if d.IsError {
				add("error")
			}
		}
	case fw.EvToolProgress:
		var d fw.ToolProgressData
		if e.Decode(&d) == nil {
			add("tool=%s progress=%g", firstNonEmpty(tools[d.CallID], d.CallID), d.Progress)
		}
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) == nil {
			add("%s=%s tool=%s", kindOf(d), d.ApprovalID, d.Tool)
		}
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) == nil {
			add("decision=%s decider=%s", d.Decision, d.Decider)
		}
	case fw.EvContextCompacted:
		var d fw.ContextCompactedData
		if e.Decode(&d) == nil {
			add("epoch=%d", d.Epoch)
		}
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) == nil {
			add("limit=%s value=%g", d.Limit, d.Value)
			if d.Scope != "" {
				add("scope=%s run=%s", d.Scope, d.Run)
			}
			if d.Report {
				add("report=true")
			}
		}
	case fw.EvBudgetChanged:
		var d fw.BudgetChangedData
		if e.Decode(&d) == nil {
			add("%s", fw.BudgetText(d.Budget))
			if d.By != "" {
				add("by=%s", d.By)
			}
		}
	case fw.EvRunReacted:
		var d fw.ReactionData
		if e.Decode(&d) == nil {
			add("reaction=%s seq=%d", d.Reaction, d.Seq)
			if d.By != "" {
				add("by=%s", d.By)
			}
			if d.Receipt {
				add("receipt=true")
			}
			if d.Removed {
				add("removed=true")
			}
		}
	case fw.EvLoopDetected:
		var d fw.LoopDetectedData
		if e.Decode(&d) == nil {
			add("tool=%s count=%d period=%d action=%s", d.Tool, d.Count, d.Period, d.Action)
		}
	case fw.EvOutcomeEvaluated:
		var d fw.OutcomeEvaluatedData
		if e.Decode(&d) == nil {
			add("verdict=%s pass=%d/%d continue=%t grader=%s", d.Verdict, d.Iteration, d.Iterations, d.Continue, d.Grader)
		}
	case fw.EvError:
		var d fw.ErrorData
		if e.Decode(&d) == nil {
			add("retryable=%t %s", d.Retryable, oneLine(d.Message))
		}
	case fw.EvRunSuspended:
		var d fw.RunSuspendedData
		if e.Decode(&d) == nil {
			add("reason=%s", d.Reason)
		}
	case fw.EvRunFinished:
		var d fw.RunFinishedData
		if e.Decode(&d) == nil {
			add("outcome=%s", d.Outcome)
			if d.By != "" {
				add("by=%s", d.By)
			}
			if d.Error != "" {
				add("error=%s", oneLine(d.Error))
			}
		}
	}
	return b.String()
}

// queuedTitle is the title of a run from its fw.run.queued data, as the
// projection makes fw.Run.Title: the metadata value fw.MetaTitle, else a
// title from the input. It is one line.
func queuedTitle(d fw.RunQueuedData) string {
	if t := strings.TrimSpace(d.Metadata[fw.MetaTitle]); t != "" {
		return oneLine(t)
	}
	return fw.TitleFrom(d.Input)
}

// runTitle is the title of a run in one line: Title, else a title from
// the input.
func runTitle(r *fw.Run) string {
	if t := strings.TrimSpace(r.Title); t != "" {
		return oneLine(t)
	}
	return fw.TitleFrom(r.Input)
}

func selector(provider, model string) string {
	if provider == "" || strings.HasPrefix(model, provider+"/") {
		return model
	}
	return provider + "/" + model
}

// oneLine joins the lines of s and cuts it at 200 characters.
func oneLine(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return string(r)
}

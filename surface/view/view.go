// Package view holds the shapes that the native API returns about agents
// and runs: the caller with its roles, agent profiles with the role of the
// caller and their abilities in plain words, run lists, file views, event
// lines, and the catalog of tools. A surface that serves the same facts in
// another form, such as the workbench, builds on these functions, so every
// surface reads one shape.
//
// The usage fields stay for the native contract. Operators read the usage
// of the runs in the Prometheus metrics.
package view

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Usage is the usage of a set of runs: how many there are, how many are
// still working, how many wait for an approval, and what they consumed.
type Usage struct {
	Runs         int     `json:"runs"`
	Active       int     `json:"active"`
	Pending      int     `json:"pending"`
	Turns        int     `json:"turns"`
	InputTokens  int     `json:"inputTokens"`
	OutputTokens int     `json:"outputTokens"`
	Tokens       int     `json:"tokens"`
	CostUSD      float64 `json:"costUSD"`
	// UnpricedTokens counts the tokens whose model has no price
	// (fw.RunUsage.UnpricedTokens). CostUSD leaves them out, so the cost is
	// partial when it is above zero.
	UnpricedTokens int `json:"unpricedTokens,omitempty"`
}

// Add counts one run.
func (u *Usage) Add(r *fw.Run) {
	u.Runs++
	if !r.State.Terminal() {
		u.Active++
	}
	if r.Pending != nil {
		u.Pending++
	}
	u.Turns += r.Usage.Turns
	u.InputTokens += r.Usage.InputTokens
	u.OutputTokens += r.Usage.OutputTokens
	u.Tokens += r.Usage.InputTokens + r.Usage.OutputTokens
	u.CostUSD += r.Usage.CostUSD
	u.UnpricedTokens += r.Usage.UnpricedTokens
}

// String is the usage on one line.
func (u Usage) String() string {
	s := fmt.Sprintf("%d runs, %d turns, %d tokens", u.Runs, u.Turns, u.Tokens)
	switch {
	case u.UnpricedTokens > 0 && u.CostUSD > 0:
		s += fmt.Sprintf(", at least $%.4f (%d tokens have no price)", u.CostUSD, u.UnpricedTokens)
	case u.UnpricedTokens > 0:
		s += fmt.Sprintf(", cost unknown (%d tokens have no price)", u.UnpricedTokens)
	case u.CostUSD > 0:
		s += fmt.Sprintf(", $%.4f", u.CostUSD)
	}
	if u.Active > 0 {
		s += fmt.Sprintf(", %d active", u.Active)
	}
	if u.Pending > 0 {
		s += fmt.Sprintf(", %d waiting for an approval", u.Pending)
	}
	return s
}

// Whoami is the caller: the principal, its role on every agent it can see,
// and what it can do beyond the agents.
type Whoami struct {
	Principal *fw.Principal     `json:"principal"`
	Roles     map[string]string `json:"roles"`
	Admin     bool              `json:"admin"`
	CanCreate bool              `json:"canCreate"`
}

// String describes the caller on a few lines.
func (w Whoami) String() string {
	var b strings.Builder
	p := w.Principal
	switch {
	case p == nil || p.Subject == "":
		b.WriteString("You are the process itself: the call carries no principal and is trusted.")
	default:
		fmt.Fprintf(&b, "You are %s", p.Subject)
		if p.Name != "" && p.Name != p.Subject {
			fmt.Fprintf(&b, " (%s)", p.Name)
		}
		if p.Kind != "" {
			fmt.Fprintf(&b, ", a %s principal", p.Kind)
		}
		if p.Issuer != "" {
			fmt.Fprintf(&b, " from %s", p.Issuer)
		}
		if len(p.Groups) > 0 {
			fmt.Fprintf(&b, ", in the groups %s", strings.Join(p.Groups, ", "))
		}
		b.WriteString(".")
	}
	if w.Admin {
		b.WriteString(" You are an admin and own every agent.")
	} else if w.CanCreate {
		b.WriteString(" You can create agents.")
	}
	names := make([]string, 0, len(w.Roles))
	for name := range w.Roles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		b.WriteString("\nYou can see no agent.")
		return b.String()
	}
	b.WriteString("\nYour role on each agent:")
	for _, name := range names {
		fmt.Fprintf(&b, "\n- %s: %s", name, w.Roles[name])
	}
	return b.String()
}

// AgentView is a profile with the role of the caller and the usage of the
// runs that the caller can see. Locked is true when the configuration
// defines the profile, so no surface can change it. Abilities says what
// the agent can do in plain words. Only Agent fills it, not Agents.
type AgentView struct {
	fw.AgentProfile
	Role string `json:"role"`
	// Usage sums the runs of the agent that the caller can see.
	//
	// Deprecated: the page no longer shows usage, and operators read the
	// Prometheus metrics. The field stays for the native and MCP
	// contracts.
	Usage     Usage     `json:"usage"`
	Locked    bool      `json:"locked,omitempty"`
	Abilities []Ability `json:"abilities,omitempty"`
}

// RoleOf is the role of the caller on an agent as the Authz of the agent
// sees it: owner when the caller can edit the profile, operator when it can
// start runs, viewer when it can read them, and none otherwise. A call
// without a principal is the process itself and owns everything. A control
// plane that does not answer gives none.
func RoleOf(ctx context.Context, a *fw.Agent, name string) string {
	acc, _, err := accessOf(ctx, a, []string{name})
	if err != nil {
		return fw.AccessNone.String()
	}
	return acc[0].role
}

// roleActions are the checks behind a role, the strongest first.
var roleActions = [...]string{fw.ActionAgentEdit, fw.ActionAgentRun, fw.ActionAgentView}

// agentAccess is what the caller can do on one agent.
type agentAccess struct {
	role string // owner, operator, viewer, or none
	view bool   // the caller can view the profile
}

// accessOf asks about every agent and every extra check in one
// AuthorizeAll call, so an external control plane gets one request per
// view. It returns the access on each agent in order and the result of each
// extra check. It returns an error that wraps fw.ErrAuthzUnavailable when
// the control plane did not answer.
func accessOf(ctx context.Context, a *fw.Agent, names []string, extra ...fw.AuthzCheck) ([]agentAccess, []error, error) {
	checks := make([]fw.AuthzCheck, 0, len(extra)+len(roleActions)*len(names))
	checks = append(checks, extra...)
	for _, name := range names {
		for _, action := range roleActions {
			checks = append(checks, fw.AuthzCheck{Action: action, Target: fw.Target{Agent: name}})
		}
	}
	errs := a.AuthorizeAll(ctx, checks)
	for _, err := range errs {
		if errors.Is(err, fw.ErrAuthzUnavailable) {
			return nil, nil, fmt.Errorf("view: %w", err)
		}
	}
	out := make([]agentAccess, len(names))
	for i := range names {
		r := errs[len(extra)+len(roleActions)*i:]
		acc := agentAccess{role: fw.AccessNone.String(), view: r[2] == nil}
		switch {
		case r[0] == nil:
			acc.role = fw.AccessOwner.String()
		case r[1] == nil:
			acc.role = fw.AccessOperator.String()
		case r[2] == nil:
			acc.role = fw.AccessViewer.String()
		}
		out[i] = acc
	}
	return out, errs[:len(extra)], nil
}

// visibleProfiles returns the profiles that the caller can view, sorted by
// name, with the access of the caller on each. It asks once for every
// profile, plus the extra checks, whose results it also returns. The
// "default" profile that stands in when no profile exists is always
// visible, as fw.Agent.ProfilesFor shows it.
func visibleProfiles(ctx context.Context, a *fw.Agent, extra ...fw.AuthzCheck) ([]fw.AgentProfile, []agentAccess, []error, error) {
	all := a.Profiles()
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.Name
	}
	acc, more, err := accessOf(ctx, a, names, extra...)
	if err != nil {
		return nil, nil, nil, err
	}
	var profiles []fw.AgentProfile
	var access []agentAccess
	for i, p := range all {
		if _, real := a.ProfileNamed(p.Name); real && !acc[i].view {
			continue
		}
		profiles = append(profiles, p)
		access = append(access, acc[i])
	}
	return profiles, access, more, nil
}

// WhoamiOf describes the caller in ctx. It returns an error that wraps
// fw.ErrAuthzUnavailable when the control plane did not answer.
func WhoamiOf(ctx context.Context, a *fw.Agent) (Whoami, error) {
	profiles, access, more, err := visibleProfiles(ctx, a,
		fw.AuthzCheck{Action: fw.ActionAdmin},
		fw.AuthzCheck{Action: fw.ActionAgentCreate},
	)
	if err != nil {
		return Whoami{}, err
	}
	w := Whoami{
		Principal: fw.PrincipalFrom(ctx),
		Roles:     map[string]string{},
		Admin:     more[0] == nil,
		CanCreate: more[1] == nil,
	}
	if w.Principal == nil {
		w.Principal = &fw.Principal{}
	}
	for i, p := range profiles {
		w.Roles[p.Name] = access[i].role
	}
	return w, nil
}

// Agents lists the profiles that the caller can see, each with the role of
// the caller and the usage of the visible runs.
func Agents(ctx context.Context, a *fw.Agent) ([]AgentView, error) {
	profiles, access, _, err := visibleProfiles(ctx, a)
	if err != nil {
		return nil, err
	}
	runs, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	byAgent := map[string]Usage{}
	for _, r := range runs {
		u := byAgent[r.Agent]
		u.Add(r)
		byAgent[r.Agent] = u
	}
	out := make([]AgentView, 0, len(profiles))
	for i, p := range profiles {
		out = append(out, AgentView{AgentProfile: p, Role: access[i].role, Usage: byAgent[p.Name], Locked: a.ProfileLocked(p.Name)})
	}
	return out, nil
}

// Agent returns one profile with the role of the caller and its usage. It
// returns fw.ErrProfileNotFound for a name that the caller cannot see, and
// an error that wraps fw.ErrAuthzUnavailable when the control plane did not
// answer.
func Agent(ctx context.Context, a *fw.Agent, name string) (AgentView, error) {
	p, real := a.ProfileNamed(name)
	if !real {
		// With no profile configured, Agents lists the built-in profile
		// "default". Agent finds the same profiles that Agents lists.
		var ok bool
		if p, ok = builtinProfile(a, name); !ok {
			return AgentView{}, fmt.Errorf("%w: %s", fw.ErrProfileNotFound, name)
		}
	}
	acc, _, err := accessOf(ctx, a, []string{name})
	if err != nil {
		return AgentView{}, err
	}
	if real && !acc[0].view {
		return AgentView{}, fmt.Errorf("%w: %s", fw.ErrProfileNotFound, name)
	}
	runs, err := a.List(ctx)
	if err != nil {
		return AgentView{}, err
	}
	v := AgentView{AgentProfile: p, Role: acc[0].role, Locked: a.ProfileLocked(name)}
	if v.Abilities, err = Abilities(ctx, a, p); err != nil {
		return AgentView{}, err
	}
	for _, r := range runs {
		if r.Agent == name {
			v.Usage.Add(r)
		}
	}
	return v, nil
}

// builtinProfile returns the profile with that name from the list of every
// profile, which holds the built-in profile "default" when no profile is
// configured.
func builtinProfile(a *fw.Agent, name string) (fw.AgentProfile, bool) {
	for _, p := range a.Profiles() {
		if p.Name == name {
			return p, true
		}
	}
	return fw.AgentProfile{}, false
}

// Filter narrows a list of runs. An empty field matches every run. The
// state "active" matches every run that is not finished. Limit bounds the
// result when it is positive.
func Filter(runs []*fw.Run, agent, state, principal string, limit int) []*fw.Run {
	state = strings.ToLower(strings.TrimSpace(state))
	out := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		if agent != "" && r.Agent != agent {
			continue
		}
		if principal != "" && r.Principal != principal {
			continue
		}
		switch {
		case state == "":
		case state == "active":
			if r.State.Terminal() {
				continue
			}
		case string(r.State) != state:
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// FilterBatch keeps the runs of one batch, by the metadata key
// fw.MetaBatch. An empty batch keeps every run.
func FilterBatch(runs []*fw.Run, batch string) []*fw.Run {
	batch = strings.TrimSpace(batch)
	if batch == "" {
		return runs
	}
	out := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		if r.Metadata[fw.MetaBatch] == batch {
			out = append(out, r)
		}
	}
	return out
}

// EventLine is one event of a run as a short line that a person reads.
type EventLine struct {
	Seq  uint64    `json:"seq"`
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	Line string    `json:"line"`
}

// Line describes an event in a few words.
func Line(e fw.Event) EventLine {
	return EventLine{Seq: e.Seq, Type: e.Type, Time: e.Time, Line: describe(e)}
}

// describe is the text of an event line. Every type gets a line, so a tail
// of the ledger reads as a story.
func describe(e fw.Event) string {
	switch e.Type {
	case fw.EvRunQueued:
		var d fw.RunQueuedData
		if e.Decode(&d) == nil {
			s := "queued for the agent " + d.Agent
			if d.Principal != "" {
				s += " by " + d.Principal
			}
			if d.Origin != "" {
				s += " over " + d.Origin
			}
			if d.Parent != "" {
				s += fmt.Sprintf(", a child of %s at depth %d", d.Parent, d.Depth)
			}
			return s
		}
	case fw.EvRunStarted:
		var d fw.RunStartedData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("started with the model %s and %d tools", d.Model, len(d.Tools))
		}
	case fw.EvMessageReceived:
		var d fw.MessageReceivedData
		if e.Decode(&d) == nil {
			s := string(d.Role) + " message" + senderOf(d)
			if d.Origin != "" {
				s += " over " + d.Origin
			}
			if t := oneLine(fw.TextOf(d.Content), 120); t != "" {
				s += ": " + t
			}
			return s
		}
	case fw.EvTurnStarted, fw.EvTurnFinished:
		var d fw.TurnData
		if e.Decode(&d) == nil {
			if e.Type == fw.EvTurnStarted {
				return fmt.Sprintf("turn %d started", d.Turn)
			}
			return fmt.Sprintf("turn %d finished", d.Turn)
		}
	case fw.EvModelRequested:
		var d fw.ModelRequestedData
		if e.Decode(&d) == nil {
			s := "asked the model " + d.Provider + "/" + d.Model
			if d.EstimatedInputTokens > 0 {
				s += fmt.Sprintf(" with about %d tokens", d.EstimatedInputTokens)
			}
			return s
		}
	case fw.EvModelResponded:
		var d fw.ModelRespondedData
		if e.Decode(&d) == nil {
			var names []string
			for _, b := range d.Content {
				if b.Type == fw.BlockToolUse {
					names = append(names, b.Name)
				}
			}
			if len(names) > 0 {
				return "the model called " + strings.Join(names, ", ")
			}
			if t := oneLine(fw.TextOf(d.Content), 120); t != "" {
				return "the model answered: " + t
			}
			return "the model stopped: " + string(d.StopReason)
		}
	case fw.EvToolRequested:
		var d fw.ToolRequestedData
		if e.Decode(&d) == nil {
			return "called the tool " + d.Tool + " with " + oneLine(string(d.Input), 100)
		}
	case fw.EvToolResponded:
		var d fw.ToolRespondedData
		if e.Decode(&d) == nil {
			tool := d.Tool
			if tool == "" {
				tool = d.CallID
			}
			if d.IsError {
				return "the tool " + tool + " failed: " + oneLine(fw.TextOf(d.Content), 120)
			}
			if d.ExitCode != nil && *d.ExitCode != 0 {
				return fmt.Sprintf("the tool %s exited %d in %d ms", tool, *d.ExitCode, d.LatencyMS)
			}
			return fmt.Sprintf("the tool %s answered in %d ms", tool, d.LatencyMS)
		}
	case fw.EvToolProgress:
		var d fw.ToolProgressData
		if e.Decode(&d) == nil {
			s := "progress"
			if d.Total > 0 {
				s += fmt.Sprintf(" %.0f of %.0f", d.Progress, d.Total)
			}
			if d.Message != "" {
				s += ": " + oneLine(d.Message, 120)
			}
			return s
		}
	case fw.EvFileChanged:
		var d fw.FileChangedData
		if e.Decode(&d) == nil {
			switch d.Op {
			case fw.FileRenamed:
				return fmt.Sprintf("the tool %s renamed %s to %s", d.Tool, d.OldPath, d.Path)
			case fw.FileRemoved:
				return fmt.Sprintf("the tool %s removed %s", d.Tool, d.Path)
			}
			return fmt.Sprintf("the tool %s %s %s (%d bytes)", d.Tool, d.Op, d.Path, d.Size)
		}
	case fw.EvMemoryChanged:
		var d fw.MemoryChangedData
		if e.Decode(&d) == nil {
			switch d.Op {
			case fw.FileRenamed:
				return fmt.Sprintf("the tool %s renamed the note %s to %s", d.Tool, d.OldRef, d.Ref)
			case fw.FileRemoved:
				return fmt.Sprintf("the tool %s removed the note %s", d.Tool, d.Ref)
			}
			return fmt.Sprintf("the tool %s %s the note %s (%d bytes)", d.Tool, d.Op, d.Ref, d.Size)
		}
	case fw.EvApprovalRequested:
		var d fw.ApprovalRequestedData
		if e.Decode(&d) == nil {
			if d.Connect != nil {
				if d.Connect.Reason == fw.ConnectExpired {
					return "waiting for the person to sign in to " + ConnectTitle(*d.Connect) + " again"
				}
				return "waiting for the person to connect " + ConnectTitle(*d.Connect)
			}
			if len(d.Schema) > 0 {
				return "the tool " + d.Tool + " asks: " + oneLine(d.Reason, 120)
			}
			s := "waiting for an approval of " + d.Tool
			if d.Reason != "" {
				s += " (" + d.Reason + ")"
			}
			return s
		}
	case fw.EvApprovalDecided:
		var d fw.ApprovalDecidedData
		if e.Decode(&d) == nil {
			s := "approval decided: " + d.Decision
			if d.Decider != "" {
				s += " by " + d.Decider
			}
			if d.Note != "" {
				s += " (" + oneLine(d.Note, 80) + ")"
			}
			return s
		}
	case fw.EvRunAnnotated:
		var d fw.RunAnnotatedData
		if e.Decode(&d) == nil {
			if t := strings.TrimSpace(d.Metadata[fw.MetaTitle]); t != "" {
				return "titled: " + oneLine(t, 80)
			}
			keys := slices.Sorted(maps.Keys(d.Metadata))
			if len(keys) == 0 {
				return "annotated with usage"
			}
			return "annotated: " + strings.Join(keys, ", ")
		}
	case fw.EvRunReacted:
		var d fw.ReactionData
		if e.Decode(&d) == nil {
			return reactionLine(d)
		}
	case fw.EvContextCompacted:
		var d fw.ContextCompactedData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("compacted the context into epoch %d", d.Epoch)
		}
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) == nil {
			s := fmt.Sprintf("budget exceeded: %s at %g", d.Limit, d.Value)
			if d.Scope == fw.ScopeTree {
				s = fmt.Sprintf("budget exceeded: %s of the run tree of %s at %g", d.Limit, d.Run, d.Value)
			}
			if d.Report {
				s += "; asking the model for a final report"
			}
			return s
		}
	case fw.EvBudgetChanged:
		var d fw.BudgetChangedData
		if e.Decode(&d) == nil {
			s := "budget changed: " + fw.BudgetText(d.Budget)
			if d.By != "" {
				s += " by " + d.By
			}
			return s
		}
	case fw.EvOutcomeEvaluated:
		var d fw.OutcomeEvaluatedData
		if e.Decode(&d) == nil {
			s := fmt.Sprintf("graded %s in pass %d of %d", d.Verdict, d.Iteration, d.Iterations)
			if d.Continue {
				s += "; the run revises its answer"
			}
			if t := oneLine(d.Notes, 120); t != "" {
				s += ": " + t
			} else if d.Reason != "" {
				s += " (" + oneLine(d.Reason, 120) + ")"
			}
			return s
		}
	case fw.EvLoopDetected:
		var d fw.LoopDetectedData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("loop detected: %s repeated %d times, %s", d.Tool, d.Count, d.Action)
		}
	case fw.EvError:
		var d fw.ErrorData
		if e.Decode(&d) == nil {
			s := "error: " + oneLine(d.Message, 160)
			if d.Retryable {
				s += " (retryable)"
			}
			return s
		}
	case fw.EvRunSuspended:
		var d fw.RunSuspendedData
		if e.Decode(&d) == nil {
			return "suspended: " + d.Reason
		}
	case fw.EvRunResumed:
		var d fw.RunResumedData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("resumed from sequence %d", d.FromSeq)
		}
	case fw.EvRunFinished:
		var d fw.RunFinishedData
		if e.Decode(&d) == nil {
			s := "finished: " + d.Outcome
			if d.Error != "" && d.Error != d.Outcome {
				s += " (" + oneLine(d.Error, 160) + ")"
			}
			return s
		}
	}
	return strings.TrimPrefix(e.Type, "fw.")
}

// senderOf names the sender of a message for its event line: " from the
// agent reviewer through token" for a message that the static token relays
// from an agent, " from alice" for a message of alice, and "" for a
// message without a sender, such as a note of the kernel.
func senderOf(d fw.MessageReceivedData) string {
	if f := d.From; f != nil {
		s := " from " + oneLine(f.Name, 60)
		if f.Kind == fw.FromAgent {
			s = " from the agent " + oneLine(f.Name, 60)
		}
		if d.By != "" && d.By != f.Name {
			s += " through " + oneLine(d.By, 60)
		}
		return s
	}
	if d.By != "" {
		return " from " + oneLine(d.By, 60)
	}
	return ""
}

// reactionLine is the event line of a reaction, such as "alice reacted
// heart to event 118" or "alice reacted up to the run". A thumb that
// recorded a quality receipt says that the person accepted or rejected
// the result, and the note follows.
func reactionLine(d fw.ReactionData) string {
	who := "the process"
	if d.By != "" {
		who = oneLine(d.By, 60)
	}
	s := who + " reacted " + d.Reaction
	if d.Seq == 0 {
		s += " to the run"
	} else {
		s += fmt.Sprintf(" to event %d", d.Seq)
	}
	if d.Receipt {
		switch d.Reaction {
		case fw.ReactionUp:
			s += " and accepted the result"
		case fw.ReactionDown:
			s += " and rejected the result"
		}
	}
	if t := oneLine(d.Note, 80); t != "" {
		s += ": " + t
	}
	return s
}

// oneLine folds the whitespace of s and cuts it to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// Tail reads the events of a run as lines, with the paging of EventsPage.
func Tail(ctx context.Context, a *fw.Agent, id string, from uint64, limit int) (lines []EventLine, next uint64, err error) {
	events, next, err := EventsPage(ctx, a, id, from, limit)
	if err != nil {
		return nil, 0, err
	}
	lines = make([]EventLine, 0, len(events))
	for _, e := range events {
		lines = append(lines, Line(e))
	}
	return lines, next, nil
}

// EventsPage reads a page of the events of a run. With from set, it
// returns the events from that sequence number on, at most limit of them,
// and next is the sequence number to continue from when more follow.
// Without from, it returns the last limit events. The caller authorizes the
// read: EventsPage reads the ledger directly.
func EventsPage(ctx context.Context, a *fw.Agent, id string, from uint64, limit int) (events []fw.Event, next uint64, err error) {
	if limit <= 0 {
		limit = 50
	}
	for e, rerr := range a.Ledger().Read(ctx, id, from) {
		if rerr != nil {
			return nil, 0, fmt.Errorf("view: read the ledger of run %s: %w", id, rerr)
		}
		events = append(events, e)
		if from > 0 && len(events) > limit {
			// One past the page: it marks where the next page starts.
			next = events[limit].Seq
			return events[:limit], next, nil
		}
	}
	if from == 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	if events == nil {
		events = []fw.Event{}
	}
	return events, 0, nil
}

// ConnectTitle is the name of the service of a connect request: its title,
// else the name of the server.
func ConnectTitle(c fw.ConnectRequest) string {
	if t := strings.TrimSpace(c.Title); t != "" {
		return t
	}
	return c.Server
}

// SortRuns sorts runs newest first, as the kernel lists them.
func SortRuns(runs []*fw.Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.After(runs[j].CreatedAt)
		}
		return runs[i].ID > runs[j].ID
	})
}

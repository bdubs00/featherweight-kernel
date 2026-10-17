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
}

// String is the usage on one line.
func (u Usage) String() string {
	s := fmt.Sprintf("%d runs, %d turns, %d tokens", u.Runs, u.Turns, u.Tokens)
	if u.CostUSD > 0 {
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
// and what it can do beyond the agents. Fleet is the member name of this
// process when the fleet is on (docs/ARCHITECTURE.md section 8.6).
type Whoami struct {
	Principal *fw.Principal     `json:"principal"`
	Teams     []string          `json:"teams,omitempty"` // the teams of the caller (section 13.6)
	Roles     map[string]string `json:"roles"`
	Admin     bool              `json:"admin"`
	CanCreate bool              `json:"canCreate"`
	Fleet     string            `json:"fleet,omitempty"`
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
	if w.Fleet != "" {
		fmt.Fprintf(&b, " This process is the member %s of a fleet; the fleet tool lists the members.", w.Fleet)
	}
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
	Role  string   `json:"role"`
	Teams []string `json:"teams,omitempty"` // the teams of the agent (section 13.6)
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
	w.Teams = a.TeamsOf(fw.PrincipalFrom(ctx))
	if f := a.Fleet(); f != nil {
		w.Fleet = f.Name()
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
		out = append(out, AgentView{AgentProfile: p, Role: access[i].role, Teams: a.AgentTeamsOf("", p.Name, p.Access), Usage: byAgent[p.Name], Locked: a.ProfileLocked(p.Name)})
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
	v := AgentView{AgentProfile: p, Role: acc[0].role, Teams: a.AgentTeamsOf("", p.Name, p.Access), Locked: a.ProfileLocked(name)}
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

// Put creates or replaces a profile for the caller and returns the saved
// view. It normalizes the profile first (see Normalize). A replaced profile
// that names no access block keeps the access block it had, so an edit
// never drops the owners by accident. A new profile without an access block
// gets the caller as its owner, in the kernel. Put reports whether the
// profile is new.
func Put(ctx context.Context, a *fw.Agent, p fw.AgentProfile) (AgentView, bool, error) {
	if err := Normalize(&p); err != nil {
		return AgentView{}, false, err
	}
	old, exists := a.ProfileNamed(p.Name)
	if exists && p.Access == nil {
		p.Access = old.Access
	}
	if err := a.PutProfile(ctx, p); err != nil {
		return AgentView{}, false, err
	}
	v, err := Agent(ctx, a, p.Name)
	return v, !exists, err
}

// Draft is the view of a profile that is not saved: what the agent would
// do, and whether saving it replaces an agent that exists.
type Draft struct {
	AgentView
	Exists bool `json:"exists"`
}

// Preview checks a profile as Put does and changes nothing. A new profile
// shows the role owner, which the caller gets when the profile names no
// access.
func Preview(ctx context.Context, a *fw.Agent, p fw.AgentProfile) (Draft, error) {
	if err := Normalize(&p); err != nil {
		return Draft{}, err
	}
	exists, err := a.CheckProfile(ctx, p)
	if err != nil {
		return Draft{}, err
	}
	d := Draft{AgentView: AgentView{AgentProfile: p, Role: "owner"}, Exists: exists}
	if exists {
		d.Role = RoleOf(ctx, a, p.Name)
	}
	if d.Abilities, err = Abilities(ctx, a, p); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// Normalize trims the profile: the name, the effort, the time zone, the
// policy default, and the access entries. It refuses, with
// fw.ErrInvalidRequest, a time zone that fw.LoadZone does not resolve and a
// policy default that is not allow, ask, or deny. The kernel checks the
// rest.
func Normalize(p *fw.AgentProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	p.Effort = strings.ToLower(strings.TrimSpace(p.Effort))
	p.Model = strings.TrimSpace(p.Model)
	p.TimeZone = strings.TrimSpace(p.TimeZone)
	if p.TimeZone != "" {
		if _, _, err := fw.LoadZone(p.TimeZone); err != nil {
			return fmt.Errorf("%w: timeZone %q is not an IANA time zone such as America/Chicago, or UTC", fw.ErrInvalidRequest, p.TimeZone)
		}
	}
	if p.Policy != nil {
		r := *p.Policy
		r.Default = strings.ToLower(strings.TrimSpace(r.Default))
		switch r.Default {
		case "", "allow", "ask", "deny":
		default:
			return fmt.Errorf("%w: policy.default %q is not allow, ask, or deny", fw.ErrInvalidRequest, p.Policy.Default)
		}
		p.Policy = &r
	}
	if p.Access != nil {
		acc := fw.Access{
			Owners:    cleanList(p.Access.Owners),
			Operators: cleanList(p.Access.Operators),
			Viewers:   cleanList(p.Access.Viewers),
		}
		p.Access = &acc
	}
	return nil
}

// cleanList trims the entries and drops the empty ones. An empty result is
// an empty list, not nil, so the access block keeps its shape.
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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

// Values of By, the part of the runs that a list keeps (section 13.6).
const (
	ByMe     = "me"     // the runs that the caller started
	ByTeam   = "team"   // and the runs that a person on a team of the caller started
	ByOthers = "others" // every other run
)

// By keeps the runs of one part, for the principal in ctx and its teams.
// An empty by keeps every run. A context without a principal is the
// process itself: it started no run and has no team.
func By(ctx context.Context, a *fw.Agent, runs []*fw.Run, by string) ([]*fw.Run, error) {
	by = strings.ToLower(strings.TrimSpace(by))
	switch by {
	case "":
		return runs, nil
	case ByMe, ByTeam, ByOthers:
	default:
		return nil, fmt.Errorf("%w: by is %q, not me, team, or others", fw.ErrInvalidRequest, by)
	}
	me, teams := "", []string(nil)
	if p := fw.PrincipalFrom(ctx); p != nil {
		me, teams = p.Subject, a.TeamsOf(p)
	}
	out := make([]*fw.Run, 0, len(runs))
	for _, r := range runs {
		mine := me != "" && r.Principal == me
		near := mine || fw.SharesTeam(r.Teams, teams)
		if (by == ByMe && mine) || (by == ByTeam && near) || (by == ByOthers && !near) {
			out = append(out, r)
		}
	}
	return out, nil
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
			s := string(d.Role) + " message"
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
	case fw.EvRunTitled:
		var d fw.RunTitledData
		if e.Decode(&d) == nil {
			return "titled: " + oneLine(d.Title, 80)
		}
	case fw.EvContextCompacted:
		var d fw.ContextCompactedData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("compacted the context into epoch %d", d.Epoch)
		}
	case fw.EvBudgetExceeded:
		var d fw.BudgetExceededData
		if e.Decode(&d) == nil {
			return fmt.Sprintf("budget exceeded: %s at %g", d.Limit, d.Value)
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

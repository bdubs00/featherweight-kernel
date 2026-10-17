package fw

// This file is authorization: who can do what with an agent and its runs.
// Authentication (surface.go) says who the caller is. The Authz plug says
// what the caller can do. The kernel asks it on every call that carries a
// principal, so every surface is covered the same way.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrForbidden is returned when the principal is known but not allowed. A
// surface answers 403.
var ErrForbidden = errors.New("fw: forbidden")

// Access names who owns and who uses an agent profile. Each entry is an
// access entry as PrincipalMatches reads it: a subject, a glob over
// subjects, group:<name>, kind:<kind>, or "*".
//
// An owner manages the profile and can do everything an operator can. An
// operator starts runs on the agent, sends follow-ups, approves, and
// cancels. A viewer reads the runs and their ledgers. The principal that
// started a run can always view and operate that run.
type Access struct {
	Owners    []string `yaml:"owners,omitempty" json:"owners,omitempty"`
	Operators []string `yaml:"operators,omitempty" json:"operators,omitempty"`
	Viewers   []string `yaml:"viewers,omitempty" json:"viewers,omitempty"`
}

// AccessConfig is the process-wide part of authorization.
//
// Default is "open" or "closed". With "open", the default, an agent that
// has no access block is open to every authenticated principal as an
// operator, which is how fw behaved before access existed. With "closed",
// such an agent is reachable by its admins only. Admins own every agent.
// Creators can create agents through the API; the one who creates an agent
// becomes its first owner. An empty Creators list means the admins, plus
// everyone when Default is open.
//
// External hands every decision to a control plane outside the process: an
// MCP tool or a bus subject (see AuthzRequest). Then fw makes no decision
// of its own: Default, Admins, and Creators do not apply, and the access
// block of a profile goes to the control plane as data in each request. A
// control plane that does not answer refuses every request with
// ErrAuthzUnavailable.
type AccessConfig struct {
	Default  string                `yaml:"default" json:"default"`
	Admins   []string              `yaml:"admins,omitempty" json:"admins,omitempty"`
	Creators []string              `yaml:"creators,omitempty" json:"creators,omitempty"`
	External *ExternalPolicyConfig `yaml:"external,omitempty" json:"external,omitempty"`
}

// Actions that the kernel authorizes.
const (
	ActionAgentView   = "agent.view"   // read the profile and list it
	ActionAgentRun    = "agent.run"    // start a run on the agent
	ActionAgentEdit   = "agent.edit"   // change the profile
	ActionAgentDelete = "agent.delete" // remove the profile
	ActionAgentCreate = "agent.create" // add a profile
	ActionRunView     = "run.view"     // read a run and its ledger
	ActionRunOperate  = "run.operate"  // send, approve, cancel
	ActionAdmin       = "admin"        // everything
)

// Target is what an action applies to. Agent names the profile. Run is set
// for an action on a run, and then Agent is the agent of the run.
//
// Member names the fleet member that owns the agent and the run when it is
// another process (section 8.6). Access and Lineage then carry the facts
// from the record of that member, because this process cannot look them
// up. The default rules refuse such a target.
type Target struct {
	Agent string
	Run   *Run

	Member  string
	Access  *Access
	Lineage []RunRef
}

// Authz decides whether a principal can do an action on a target. It
// returns nil to allow, and ErrForbidden, wrapped with a reason, to refuse.
type Authz interface {
	Allow(ctx context.Context, p *Principal, action string, t Target) error
}

// AuthzFunc adapts a function to Authz.
type AuthzFunc func(ctx context.Context, p *Principal, action string, t Target) error

// Allow implements Authz.
func (f AuthzFunc) Allow(ctx context.Context, p *Principal, action string, t Target) error {
	return f(ctx, p, action, t)
}

// AccessRole is the role of a principal on an agent.
type AccessRole int

const (
	AccessNone AccessRole = iota
	AccessViewer
	AccessOperator
	AccessOwner
)

// String implements fmt.Stringer.
func (r AccessRole) String() string {
	switch r {
	case AccessViewer:
		return "viewer"
	case AccessOperator:
		return "operator"
	case AccessOwner:
		return "owner"
	}
	return "none"
}

// AccessRoleOf returns the role of a principal under an access block. A
// nil block gives AccessNone.
func AccessRoleOf(acc *Access, p *Principal) AccessRole {
	if acc == nil || p == nil {
		return AccessNone
	}
	switch {
	case MatchesAny(acc.Owners, p):
		return AccessOwner
	case MatchesAny(acc.Operators, p):
		return AccessOperator
	case MatchesAny(acc.Viewers, p):
		return AccessViewer
	}
	return AccessNone
}

// NewRulesAuthz builds the default Authz from the access configuration.
// The profile function returns the current profile of an agent, so a
// profile changed at runtime takes effect at once.
func NewRulesAuthz(cfg AccessConfig, profile func(name string) (AgentProfile, bool)) Authz {
	open := strings.ToLower(strings.TrimSpace(cfg.Default)) != "closed"
	return AuthzFunc(func(_ context.Context, p *Principal, action string, t Target) error {
		if p == nil {
			return nil
		}
		deny := func(need string) error {
			return fmt.Errorf("%w: %s %s needs %s", ErrForbidden, p.Subject, action, need)
		}
		if t.Member != "" {
			// The rules of this process say nothing about another process.
			return deny("a decision point that knows the fleet member " + t.Member)
		}
		if MatchesAny(cfg.Admins, p) {
			return nil
		}
		switch action {
		case ActionAdmin:
			return deny("an entry in access.admins")
		case ActionAgentCreate:
			if MatchesAny(cfg.Creators, p) || (len(cfg.Creators) == 0 && open) {
				return nil
			}
			return deny("an entry in access.creators")
		}
		agent := t.Agent
		if agent == "" && t.Run != nil {
			agent = t.Run.Agent
		}
		role := AccessNone
		if prof, ok := profile(agent); ok && prof.Access != nil {
			role = AccessRoleOf(prof.Access, p)
		} else if open {
			role = AccessOperator
		}
		own := t.Run != nil && t.Run.Principal != "" && t.Run.Principal == p.Subject
		switch action {
		case ActionAgentView:
			if role >= AccessViewer {
				return nil
			}
			return deny("a viewer entry on the agent " + agent)
		case ActionAgentRun:
			if role >= AccessOperator {
				return nil
			}
			return deny("an operator entry on the agent " + agent)
		case ActionAgentEdit, ActionAgentDelete:
			if role >= AccessOwner {
				return nil
			}
			return deny("an owner entry on the agent " + agent)
		case ActionRunView:
			if own || role >= AccessViewer {
				return nil
			}
			return deny("a viewer entry on the agent " + agent + ", or to be the one who started the run")
		case ActionRunOperate:
			if own || role >= AccessOperator {
				return nil
			}
			return deny("an operator entry on the agent " + agent + ", or to be the one who started the run")
		}
		return deny("a known action")
	})
}

// AuthzCheck is one action on one target.
type AuthzCheck struct {
	Action string
	Target Target
}

// BatchAuthz is an Authz that decides several checks for one principal at
// once, such as the runs of a listing. AllowAll returns one error per
// check, in order, and nil allows the check.
type BatchAuthz interface {
	AllowAll(ctx context.Context, p *Principal, checks []AuthzCheck) []error
}

// lineageAuthz is an Authz that decides about a child run from its lineage
// by itself, so Authorize does not ask again for the ancestors.
type lineageAuthz interface{ decidesLineage() }

// Authorize asks the Authz of the agent whether the principal in ctx can do
// the action. A context without a principal is a call from the process
// itself and is allowed. An action on a child run is also allowed when the
// same action is allowed on an ancestor, so whoever sees a run sees the
// whole tree under it. An external decision point (access.external)
// decides that rule itself from the lineage in its request.
func (a *Agent) Authorize(ctx context.Context, action string, t Target) error {
	return a.AuthorizeAll(ctx, []AuthzCheck{{Action: action, Target: t}})[0]
}

// AuthorizeAll is Authorize for several checks. It returns one error per
// check, in order. An Authz that implements BatchAuthz gets every check in
// one call.
func (a *Agent) AuthorizeAll(ctx context.Context, checks []AuthzCheck) []error {
	out := make([]error, len(checks))
	p := PrincipalFrom(ctx)
	if p == nil || a.authz == nil || len(checks) == 0 {
		return out
	}
	if b, ok := a.authz.(BatchAuthz); ok {
		copy(out, b.AllowAll(ctx, p, checks))
	} else {
		for i, c := range checks {
			out[i] = a.authz.Allow(ctx, p, c.Action, c.Target)
		}
	}
	if _, ok := a.authz.(lineageAuthz); ok {
		return out
	}
	for i, c := range checks {
		if c.Target.Member != "" {
			continue // the ancestors live in another process
		}
		for cur := c.Target.Run; out[i] != nil && !errors.Is(out[i], ErrAuthzUnavailable) && cur != nil && cur.Parent != "" && a.k != nil; {
			rs, err := a.k.lookup(cur.Parent)
			if err != nil {
				break
			}
			cur = rs.snapshot()
			if a.authz.Allow(ctx, p, c.Action, Target{Agent: cur.Agent, Run: cur}) == nil {
				out[i] = nil
			}
		}
	}
	return out
}

// Authz returns the authorization plug.
func (a *Agent) Authz() Authz { return a.authz }

// How a process authorizes, as AccessMode reports it.
const (
	AccessModeRules    = "rules"    // the access rules of section 13.2
	AccessModeExternal = "external" // access.external decides
	AccessModeCustom   = "custom"   // an Authz from WithAuthz decides
)

// AccessMode names how this process authorizes: AccessModeRules,
// AccessModeExternal, or AccessModeCustom. A linked package that tells
// other processes about this one, such as the fleet of the workbench,
// publishes it.
func (a *Agent) AccessMode() string { return a.accessMode }

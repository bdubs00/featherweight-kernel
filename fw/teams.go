package fw

// This file is teams (docs/ARCHITECTURE.md section 13.6): groups of people
// and agents that work together. A team groups the view and grants no
// access. With the rules of section 13.2, the teams come from the groups
// of a principal and from the group: entries of an access block. With
// access.external, they come from the control plane alone.

import (
	"slices"
	"strings"
)

// TeamTeller is an Authz that knows the teams of principals and agents,
// such as the control plane of access.external. Its answers replace the
// rules of this file. A nil slice means that it does not know the teams.
type TeamTeller interface {
	PrincipalTeams(p *Principal) []string
	AgentTeams(member, agent string) []string
}

// TeamsOf returns the teams of a principal, sorted. A context without a
// principal, which is the process itself, has no team.
func (a *Agent) TeamsOf(p *Principal) []string {
	if p == nil || p.Subject == "" {
		return nil
	}
	if t, ok := a.authz.(TeamTeller); ok {
		return sortedTeams(t.PrincipalTeams(p))
	}
	var out []string
	for _, g := range p.Groups {
		if a.isTeam(g) {
			out = append(out, g)
		}
	}
	return sortedTeams(out)
}

// AgentTeamsOf returns the teams of an agent, sorted. Member names the
// fleet member of the agent when it is another process, and access is its
// access block. Without a control plane, the teams are the groups that the
// access block names exactly, as group:<name>.
func (a *Agent) AgentTeamsOf(member, agent string, access *Access) []string {
	if t, ok := a.authz.(TeamTeller); ok {
		return sortedTeams(t.AgentTeams(member, agent))
	}
	if access == nil {
		return nil
	}
	var out []string
	for _, list := range [][]string{access.Owners, access.Operators, access.Viewers} {
		for _, e := range list {
			g, ok := strings.CutPrefix(e, "group:")
			if ok && g != "" && !strings.ContainsAny(g, "*?[") && a.isTeam(g) {
				out = append(out, g)
			}
		}
	}
	return sortedTeams(out)
}

// isTeam reports whether a group counts as a team: it matches a pattern of
// access.teams, and every group does when the list is empty.
func (a *Agent) isTeam(group string) bool {
	if len(a.cfg.Access.Teams) == 0 {
		return true
	}
	for _, p := range a.cfg.Access.Teams {
		if globMatch(p, group) {
			return true
		}
	}
	return false
}

// SharesTeam reports whether two lists of teams have a team in common.
func SharesTeam(a, b []string) bool {
	for _, t := range a {
		if slices.Contains(b, t) {
			return true
		}
	}
	return false
}

// sortedTeams returns the teams sorted, without duplicates or empty names.
func sortedTeams(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

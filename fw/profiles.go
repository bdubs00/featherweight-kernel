package fw

// This file is the agent profiles at runtime. The configuration loads them
// at start, from fw.yaml and from the files under <home>/agents. A linked
// package that manages profiles for people, such as the workbench, writes
// the files and hands the kernel the new set with SetProfiles. The kernel
// writes no profile itself. A profile that the configuration defines, in
// fw.yaml, in a FW_AGENTS_* variable, or through a Go option, is fixed:
// the configuration wins at the next start, so SetProfiles refuses a
// change to it.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
)

// ErrProfileNotFound is returned by DeleteProfile for an unknown name.
var ErrProfileNotFound = fmt.Errorf("%w: profile not found", ErrAgentNotFound)

// profiles returns the current profile map. The map is replaced, never
// changed in place, so a reader keeps a consistent view.
func (a *Agent) profiles() map[string]AgentProfile {
	a.profMu.RLock()
	defer a.profMu.RUnlock()
	return a.cfg.Agents
}

// profileNamed returns the profile with that name, or the default profile
// as Config.ProfileNamed does.
func (a *Agent) profileNamed(name string) AgentProfile {
	if name == "" {
		name = "default"
	}
	if p, ok := a.profiles()[name]; ok {
		p.Name = name
		return p
	}
	return AgentProfile{Name: name}
}

// ProfileNamed returns the profile with that name and whether it exists.
func (a *Agent) ProfileNamed(name string) (AgentProfile, bool) {
	p, ok := a.profiles()[name]
	if ok {
		p.Name = name
	}
	return p, ok
}

// Profiles lists the agent profiles, sorted by name. With a principal in
// ctx, it lists the profiles that the principal can view.
func (a *Agent) Profiles() []AgentProfile {
	return a.ProfilesFor(context.Background())
}

// ProfilesFor lists the profiles that the principal in ctx can view, sorted
// by name. Without a principal it lists every profile. It lists none when
// the decision point does not answer. ListProfiles returns that error.
func (a *Agent) ProfilesFor(ctx context.Context) []AgentProfile {
	out, _ := a.ListProfiles(ctx)
	return out
}

// ListProfiles is ProfilesFor with an error: it wraps ErrAuthzUnavailable
// when the decision point does not answer.
func (a *Agent) ListProfiles(ctx context.Context) ([]AgentProfile, error) {
	all := a.profiles()
	names := make([]string, 0, len(all))
	checks := make([]AuthzCheck, 0, len(all))
	for name := range all {
		names = append(names, name)
		checks = append(checks, AuthzCheck{Action: ActionAgentView, Target: Target{Agent: name}})
	}
	errs := a.AuthorizeAll(ctx, checks)
	out := make([]AgentProfile, 0, len(all))
	for i, name := range names {
		if errors.Is(errs[i], ErrAuthzUnavailable) {
			return nil, errs[i]
		}
		if errs[i] != nil {
			continue
		}
		p := all[name]
		p.Name = name
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 && len(all) == 0 {
		out = append(out, AgentProfile{Name: "default"})
	}
	return out, nil
}

// ProfileSet returns a copy of the current profiles, by name.
func (a *Agent) ProfileSet() map[string]AgentProfile {
	return maps.Clone(a.profiles())
}

// SetProfiles replaces the profiles of the agent with next. A linked package
// that keeps profiles on disk for people, such as the workbench, calls it
// after a write, so a run can name the new profile at once. Every profile
// must pass CheckProfile. A profile that the configuration defines must
// stay as it is, or SetProfiles refuses the set with ErrInvalidRequest,
// because the configuration wins at the next start. Runs in flight keep
// the profile that they started with.
func (a *Agent) SetProfiles(next map[string]AgentProfile) error {
	set := make(map[string]AgentProfile, len(next))
	for name, p := range next {
		p.Name = strings.TrimSpace(name)
		if err := a.CheckProfile(p); err != nil {
			return err
		}
		set[p.Name] = p
	}
	a.profMu.Lock()
	defer a.profMu.Unlock()
	for name := range a.fixed {
		cur, ok := a.cfg.Agents[name]
		if !ok {
			continue // a fixed profile that an option removed is gone for good
		}
		cur.Name = name
		if p, ok := set[name]; !ok || !reflect.DeepEqual(p, cur) {
			return fmt.Errorf("%w: the configuration defines the agent %s, so change it there", ErrInvalidRequest, name)
		}
	}
	a.cfg.Agents = set
	return nil
}

// CheckProfile reports whether the kernel accepts a profile: its name is
// safe as a file name, a tool name part, and a URL element, and its fields
// fit, as checkProfile checks an inline profile. It changes nothing.
func (a *Agent) CheckProfile(p AgentProfile) error {
	if err := checkProfileName(strings.TrimSpace(p.Name)); err != nil {
		return err
	}
	return a.checkProfile(&p)
}

// ProfileLocked reports whether the configuration defines the profile, so a
// change at runtime is lost at the next start, and a surface says so before
// a person edits it. The profiles that the loader read from the files under
// <home>/agents (Config.FileProfiles) are not locked.
func (a *Agent) ProfileLocked(name string) bool {
	a.profMu.RLock()
	defer a.profMu.RUnlock()
	_, ok := a.cfg.Agents[name]
	return ok && a.fixed[name]
}

// checkProfileName accepts names that are safe as a file name, a tool name
// part, and a URL element: letters, digits, '-', '_', and '.', at most 64
// bytes, and no leading '.'.
func checkProfileName(name string) error {
	if name == "" || len(name) > 64 || name[0] == '.' {
		return fmt.Errorf("%w: invalid profile name %q", ErrInvalidRequest, name)
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("%w: invalid profile name %q", ErrInvalidRequest, name)
		}
	}
	return nil
}

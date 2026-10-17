package fw

// This file manages the agent profiles at runtime. The configuration loads
// them at start, and the workbench or the API adds, changes, and removes
// them while the process runs. A ProfileStore keeps a change on disk, so
// the next start sees it. The config package provides the store that
// writes one YAML file per profile under <home>/agents.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ProfileStore keeps profile changes. Put writes or replaces a profile, and
// Delete removes one.
type ProfileStore interface {
	Put(ctx context.Context, p AgentProfile) error
	Delete(ctx context.Context, name string) error
}

// ProfileStoreOpener opens the profile store of a configuration. The config
// package sets it. When it is nil, a change lives in memory only and the
// kernel logs a warning once.
var ProfileStoreOpener func(cfg *Config) (ProfileStore, error)

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

// PutProfile adds or replaces a profile and keeps it in the store. A new
// profile needs ActionAgentCreate, and a change needs ActionAgentEdit. When
// a principal creates a profile that names no owner, the principal becomes
// its owner.
func (a *Agent) PutProfile(ctx context.Context, p AgentProfile) error {
	p.Name = strings.TrimSpace(p.Name)
	exists, action, err := a.checkPut(ctx, &p)
	if err != nil {
		return err
	}
	if principal := PrincipalFrom(ctx); principal != nil && !exists && p.Access == nil && principal.Kind != KindAnonymous {
		p.Access = &Access{Owners: []string{principal.Subject}}
	}
	if a.profileStore != nil {
		if err := a.profileStore.Put(ctx, p); err != nil {
			return fmt.Errorf("fw: store profile %s: %w", p.Name, err)
		}
	} else {
		a.warnNoStore()
	}
	a.profMu.Lock()
	next := make(map[string]AgentProfile, len(a.cfg.Agents)+1)
	for k, v := range a.cfg.Agents {
		next[k] = v
	}
	next[p.Name] = p
	a.cfg.Agents = next
	a.profMu.Unlock()
	a.log.Info("profile saved", "profile", p.Name, "action", action, "principal", subjectOf(ctx))
	return nil
}

// checkPut checks a profile for PutProfile: the name, the fields, and the
// authorization of a create or an edit.
func (a *Agent) checkPut(ctx context.Context, p *AgentProfile) (exists bool, action string, err error) {
	if err := checkProfileName(p.Name); err != nil {
		return false, "", err
	}
	if err := a.checkProfile(p); err != nil {
		return false, "", err
	}
	_, exists = a.profiles()[p.Name]
	action = ActionAgentCreate
	if exists {
		action = ActionAgentEdit
	}
	if err := a.Authorize(ctx, action, Target{Agent: p.Name}); err != nil {
		return false, "", err
	}
	return exists, action, nil
}

// CheckProfile runs the checks of PutProfile and changes nothing. It also
// refuses a profile that the store locks. It reports whether a profile of
// that name exists, so a surface can preview a create or an edit.
func (a *Agent) CheckProfile(ctx context.Context, p AgentProfile) (exists bool, err error) {
	p.Name = strings.TrimSpace(p.Name)
	exists, _, err = a.checkPut(ctx, &p)
	if err != nil {
		return false, err
	}
	if a.ProfileLocked(p.Name) {
		return false, fmt.Errorf("%w: the configuration defines the agent %s, so change it there", ErrInvalidRequest, p.Name)
	}
	return exists, nil
}

// DeleteProfile removes a profile. It needs ActionAgentDelete.
func (a *Agent) DeleteProfile(ctx context.Context, name string) error {
	if _, ok := a.profiles()[name]; !ok {
		return fmt.Errorf("%w: %s", ErrProfileNotFound, name)
	}
	if err := a.Authorize(ctx, ActionAgentDelete, Target{Agent: name}); err != nil {
		return err
	}
	if a.profileStore != nil {
		if err := a.profileStore.Delete(ctx, name); err != nil {
			return fmt.Errorf("fw: delete profile %s: %w", name, err)
		}
	} else {
		a.warnNoStore()
	}
	a.profMu.Lock()
	next := make(map[string]AgentProfile, len(a.cfg.Agents))
	for k, v := range a.cfg.Agents {
		if k != name {
			next[k] = v
		}
	}
	a.cfg.Agents = next
	a.profMu.Unlock()
	a.log.Info("profile deleted", "profile", name, "principal", subjectOf(ctx))
	return nil
}

func (a *Agent) warnNoStore() {
	a.storeWarn.Do(func() {
		a.log.Warn("no profile store is configured; profile changes live in memory until the process ends")
	})
}

func subjectOf(ctx context.Context) string {
	if p := PrincipalFrom(ctx); p != nil {
		return p.Subject
	}
	return ""
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

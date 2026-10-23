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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// resolveProfile resolves the configuration of a new run once, when the
// kernel queues it, from named, the profile that the caller read once for
// the whole request. It returns the record for fw.run.queued
// (RunQueuedData.Resolved), which the run keeps for its life (profileOf);
// pinText adds the digests of the text. The record is
// the overlay of the inline profile on named, with these values filled in,
// so that a later change of the configuration does not reach the run
// either:
//
//   - Model: the selector, through models and the model of the
//     configuration.
//   - Effort: the effort of the configuration when the profile has none.
//   - TimeZone: the zone of the parent run, else time.zone, when the
//     profile has none. fw.run.started records the zone that wins.
//   - Budget: the budget of the profile, with every zero field taken from
//     the budget of the configuration. The budget of the request stays in
//     RunQueuedData.Budget and wins over it.
//
// Skills, Tools, and Policy are those of the overlay. The record holds no
// configured text, only the digests of the text, and no access: who can
// use the agent is authorization, and the kernel always reads it from the
// profile now. The base policy of the process, the tools that are
// connected, and the skills that are installed are not part of a profile,
// so the kernel does not resolve them here.
func (a *Agent) resolveProfile(ctx context.Context, req RunRequest, named AgentProfile, parent *runState) *ResolvedProfile {
	prof := named
	if req.Profile != nil {
		prof = prof.Overlay(*req.Profile)
	}
	res := &ResolvedProfile{
		Model:    a.cfg.ModelSelector(prof.Model),
		Effort:   prof.Effort,
		Skills:   prof.Skills,
		Tools:    prof.Tools,
		TimeZone: prof.TimeZone,
		Policy:   prof.Policy,
		Routines: prof.Routines,
	}
	if res.Effort == "" {
		res.Effort = a.cfg.Effort
	}
	if res.TimeZone == "" && parent != nil {
		res.TimeZone = a.zoneNameOf(ctx, parent)
	}
	if res.TimeZone == "" {
		res.TimeZone = a.cfg.Time.Zone
	}
	b := a.cfg.Budget
	if prof.Budget != nil {
		b = prof.Budget.Merge(b)
	}
	res.Budget = &b
	res.Compaction = a.resolveCompaction(prof.Compaction)
	return res
}

// resolveCompaction returns the compaction setting of a new run: the block
// of its profile over the block of the configuration, with the model as a
// selector, so a later change of models or of the configuration does not
// reach the run.
func (a *Agent) resolveCompaction(p *CompactionConfig) *CompactionConfig {
	c := a.processCompaction()
	if p != nil {
		switch {
		case p.Off:
			c.Off = true
		case p.At > 0:
			c.Off, c.At = false, p.At
		}
		if p.Model != "" {
			c.Model = p.Model
		}
	}
	if c.Model != "" {
		c.Model = a.cfg.ModelSelector(c.Model)
	}
	if at, on := c.on(); on {
		c.At = at
	} else {
		c.Off, c.At = true, 0
	}
	return &c
}

// pinText returns the text of a new run, which stays in memory (textOf),
// and sets its digests in q.Resolved. The text of the inline profile
// reaches the model as fw.run.queued stores it, so pinText takes it from q
// with every secret replaced, as appendLocked replaces them in the event.
func (a *Agent) pinText(named AgentProfile, q *RunQueuedData) *runText {
	inline := q.Profile
	if raw, err := json.Marshal(q); err == nil {
		var stored RunQueuedData
		if json.Unmarshal(a.redact.data(raw), &stored) == nil {
			inline = stored.Profile
		}
	}
	text := composeText(named, inline)
	q.Resolved.SystemDigest, q.Resolved.InstructionsDigest = text.digests()
	return text
}

// profile returns the resolved profile as an AgentProfile of the agent,
// without text, for the code that reads a profile.
func (r *ResolvedProfile) profile(agent string) AgentProfile {
	return AgentProfile{Name: agent, Skills: r.Skills, Tools: r.Tools, Model: r.Model, Effort: r.Effort,
		TimeZone: r.TimeZone, Policy: r.Policy, Budget: r.Budget, Routines: r.Routines}
}

// selectorOf returns the model selector of a run. A resolved profile holds
// the selector itself, so a later change of models or of the model of the
// configuration does not change it.
func (a *Agent) selectorOf(q RunQueuedData, prof AgentProfile) string {
	if q.Resolved != nil && prof.Model != "" {
		return prof.Model
	}
	return a.cfg.ModelSelector(prof.Model)
}

// runText is the text of a run that its system prompt holds: the system
// prompt of its profile, DefaultSystemPrompt when it has none, and the
// instructions of its profile. The instructions of the request are in
// RunQueuedData.Instructions. The kernel keeps a runText in memory only,
// because configured text never enters an event.
type runText struct {
	system       string
	instructions []string
}

// composeText returns the text of a run from the named profile and the
// inline profile, as Overlay composes them.
func composeText(named AgentProfile, inline *AgentProfile) *runText {
	p := AgentProfile{System: named.System, Instructions: named.Instructions}
	if inline != nil {
		p = p.Overlay(AgentProfile{System: inline.System, Instructions: inline.Instructions})
	}
	if strings.TrimSpace(p.System) == "" {
		p.System = DefaultSystemPrompt
	}
	return &runText{system: p.System, instructions: p.Instructions}
}

// digests returns the digests of the text as the model reads it, as
// ResolvedProfile describes them.
func (t *runText) digests() (system, instructions string) {
	return textDigest(strings.TrimSpace(t.system)), textDigest(joinInstructions(t.instructions))
}

func textDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// textOf returns the text of a run with a resolved profile. A run that this
// process queued keeps the text that it was queued with in memory, so a
// change of the profile does not reach it. A branch in this process takes
// the text of the nearest run up its chain of sources (MetaForkOf) that
// holds a text with the same digests, so a branch of a branch that made no
// model call yet keeps the text of the first source too. A request can name
// any run in MetaForkOf, but the digests must agree, so a run only takes a
// text that equals its own. Otherwise, such as after a restart, the kernel
// composes the text from the named profile now and the inline profile of
// fw.run.queued and keeps it from then on.
// When its digests differ from those that the run expects, the
// configuration changed: the run goes on with the text now, and the kernel
// records the change on the run as fw.run.annotated with MetaDriftSystem
// or MetaDriftInstructions, the digest that the run expected, and the
// digest of the text now. An earlier record of a change sets the digest
// that the run expects, so each change is recorded once.
func (a *Agent) textOf(ctx context.Context, rs *runState, q RunQueuedData) (*runText, error) {
	rs.mu.Lock()
	t := rs.text
	meta := rs.run.Metadata
	rs.mu.Unlock()
	if t != nil {
		return t, nil
	}
	want := map[string]string{MetaDriftSystem: q.Resolved.SystemDigest, MetaDriftInstructions: q.Resolved.InstructionsDigest}
	for key := range want {
		if _, now, ok := strings.Cut(meta[key], " -> "); ok {
			want[key] = now
		}
	}
	agree := func(t *runText) map[string]string {
		sys, ins := t.digests()
		changed := map[string]string{}
		if sys != want[MetaDriftSystem] {
			changed[MetaDriftSystem] = want[MetaDriftSystem] + " -> " + sys
		}
		if ins != want[MetaDriftInstructions] {
			changed[MetaDriftInstructions] = want[MetaDriftInstructions] + " -> " + ins
		}
		return changed
	}
	seen := map[*runState]bool{rs: true}
	for id := q.Metadata[MetaForkOf]; id != "" && t == nil; {
		src, err := a.k.lookup(id)
		if err != nil || seen[src] {
			break
		}
		seen[src] = true
		src.mu.Lock()
		st := src.text
		src.mu.Unlock()
		if st != nil && len(agree(st)) == 0 {
			t = st
		}
		id = src.queuedData().Metadata[MetaForkOf]
	}
	if t == nil {
		t = composeText(a.profileNamed(q.Agent), q.Profile)
		if changed := agree(t); len(changed) > 0 {
			if _, err := a.emit(ctx, rs, EvRunAnnotated, RunAnnotatedData{Metadata: changed}); err != nil {
				return nil, err
			}
			a.log.Warn("the profile text of a run changed since it was queued; the run goes on with the text now",
				"run", rs.id, "agent", q.Agent, "changes", changed)
		}
	}
	rs.mu.Lock()
	rs.text = t
	rs.mu.Unlock()
	return t, nil
}

// tighten returns the resolved rules of a run with the rules that its named
// profile has now on top. The rules now can only make a verdict stricter: a
// rule that a person adds after the run started reaches the run at once,
// and a rule that they remove stays in force for the run. Clauses that the
// resolved rules already hold add nothing, so an unchanged profile leaves
// the rules as they are.
func tighten(resolved, now *Rules) *Rules {
	if now == nil {
		return resolved
	}
	if resolved == nil {
		r := *now
		return &r
	}
	held := map[string]bool{}
	for _, c := range rulesClauses(*resolved) {
		b, _ := json.Marshal(c)
		held[string(b)] = true
	}
	for _, c := range rulesClauses(*now) {
		if b, _ := json.Marshal(c); !held[string(b)] {
			r := MergeRules(*resolved, *now)
			return &r
		}
	}
	return resolved
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
// because the configuration wins at the next start. A run that the kernel
// queued before keeps the profile that it was queued with
// (RunQueuedData.Resolved): only the policy of the new profile reaches it,
// as a tightening, and access always follows the new set.
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

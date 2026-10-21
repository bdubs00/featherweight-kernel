package fw

// SkillsOfRun returns the skills that the profile of a run allows: the
// skills of Skills that the skills allowlist of the profile matches
// (SkillAllowed), the same skills that the system prompt of the run lists.
// The profile is the one that the kernel resolved when it queued the run
// named info.RunID. For a run that this process does not know, such as a
// search over the native API, it is the profile named info.Agent. The tool
// recall reads skills for a run through it.
func (a *Agent) SkillsOfRun(info RunInfo) []Skill {
	var globs []string
	if rs, err := a.k.lookup(info.RunID); info.RunID != "" && err == nil {
		globs = a.profileOf(rs.queuedData()).Skills
	} else {
		globs = a.profileNamed(info.Agent).Skills
	}
	all := a.Skills()
	out := make([]Skill, 0, len(all))
	for _, s := range all {
		if SkillAllowed(globs, s.Name) {
			out = append(out, s)
		}
	}
	return out
}

package fw

import (
	"slices"
	"strings"
	"testing"
)

// withPromptSection registers fn for the length of a test.
func withPromptSection(t *testing.T, fn PromptSection) {
	t.Helper()
	promptSections.mu.Lock()
	saved := slices.Clone(promptSections.list)
	promptSections.mu.Unlock()
	RegisterPromptSection(fn)
	t.Cleanup(func() {
		promptSections.mu.Lock()
		promptSections.list = saved
		promptSections.mu.Unlock()
	})
}

// A registered section follows the skills, with a blank line before it,
// and a section that returns only space adds nothing.
func TestSystemPromptSections(t *testing.T) {
	script := newScript(endTurn("done"))
	a := newTestAgent(t, script.Model())
	withPromptSection(t, func(got *Agent, prof AgentProfile) string {
		if got != a {
			return ""
		}
		return "# Environment\n\nPython 3.12 for " + prof.Name + ".\n"
	})
	withPromptSection(t, func(*Agent, AgentProfile) string { return "  \n" })
	got := a.SystemPrompt(AgentProfile{Name: "worker", System: "Be brief.", Instructions: []string{"Cite files."}}, "Only docs.")
	want := "Be brief." + instructionsHeading + "Cite files." + runHeading + "Only docs." + "\n\n# Environment\n\nPython 3.12 for worker."
	if got != want {
		t.Fatalf("SystemPrompt:\n%q\nwant\n%q", got, want)
	}
	other := newTestAgent(t, newScript().Model())
	if s := other.SystemPrompt(AgentProfile{System: "Be brief."}, ""); strings.Contains(s, "Environment") {
		t.Fatalf("a section of another agent leaked: %q", s)
	}
	// A model call of a run carries the section.
	if _, err := a.Run(testCtx(t), "hello"); err != nil {
		t.Fatal(err)
	}
	script.mu.Lock()
	defer script.mu.Unlock()
	if len(script.reqs) != 1 || !strings.Contains(script.reqs[0].System, "# Environment") {
		t.Fatalf("requests = %d, system = %q", len(script.reqs), script.reqs[0].System)
	}
}

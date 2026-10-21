package env

import (
	"fmt"
	"path"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// briefPackages is how many packages of one manager the brief names.
const briefPackages = 30

// section is the fw.PromptSection of the environment: what is installed
// with the versions, the manager to use, the paths, and what a Task does
// not have. It reads the state files and runs no command, so the text
// changes only when the state changes: from installing to ready, or to
// failed. environment.brief: false turns it off.
func section(a *fw.Agent, prof fw.AgentProfile) string {
	s, err := load(a.Config())
	if err != nil || s == nil || !s.brief {
		return ""
	}
	return s.briefText(s.state(), toolAllowed(prof.Tools), ax.Detect())
}

// toolAllowed reports whether a tool list of a profile names the tool env,
// as the kernel matches it: an empty list and "*" give every tool that is
// not opt-in, and the tool is not opt-in with the block.
func toolAllowed(globs []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if ok, err := path.Match(g, Name); g == "*" || (err == nil && ok) {
			return true
		}
	}
	return false
}

// briefText renders the section. A name or a version that holds anything
// but the characters of a package name is left out, because the state
// files are on a volume that other processes write.
func (s *spec) briefText(st state, canChange, inTask bool) string {
	var b strings.Builder
	b.WriteString("# Environment\n\n")
	if s.placement == PlacementImage {
		fmt.Fprintf(&b, "The packages of this agent are part of the image, in %s.\n", s.dir)
	} else {
		fmt.Fprintf(&b, "The packages of this agent live in %s. ", s.dir)
		if inTask {
			b.WriteString("That directory is on the durable volume of the Task, so the packages stay after a suspend and a restart.")
		} else {
			b.WriteString("The shell finds their programs first on PATH.")
		}
		b.WriteString("\n")
	}
	m := st.Marker
	if st.Kind != StateReady {
		m = nil
	}
	switch st.Kind {
	case StatePending, StateMaterializing:
		fmt.Fprintf(&b, "\nThe environment is being installed from %s now. The first command of the shell waits until it is ready.\n", s.manifests())
	case StateFailed:
		fmt.Fprintf(&b, "\nThe install of the environment failed at the step %s, and it tries again on its own. Until it succeeds, a shell command fails with the reason. If that blocks the task, say so in your answer.\n", safeWord(st.Failure.Step))
	}
	b.WriteString("\n")
	if s.python != "" {
		fmt.Fprintf(&b, "- Python: the virtual environment %s, from %s and %s, with uv as the manager. VIRTUAL_ENV is set, and `python` and the programs of the packages are on PATH.", s.venv(), s.rel(s.python), s.rel(s.uvLock()))
		if m != nil && m.Python != nil {
			if v := safeWord(m.Python.Version); v != "" {
				fmt.Fprintf(&b, " Python %s.", v)
			}
			if list := packageList(m.Python.Packages, false, briefPackages); list != "" {
				fmt.Fprintf(&b, " Packages: %s.", list)
			}
		}
		b.WriteString("\n")
	}
	if s.tools != "" {
		fmt.Fprintf(&b, "- Tools: programs and libraries from conda-forge, from %s and %s, with pixi as the manager. Their programs are on PATH.", s.rel(s.tools), s.rel(s.pixiLock()))
		if m != nil && m.Tools != nil {
			if list := packageList(m.Tools.Packages, true, briefPackages); list != "" {
				fmt.Fprintf(&b, " Packages: %s.", list)
			}
		}
		b.WriteString("\n")
	}
	if s.python != "" || s.tools != "" {
		switch {
		case s.placement == PlacementImage:
			b.WriteString("- The packages are fixed in the image. If one is missing, say so in your answer.\n")
		case canChange:
			b.WriteString("- To add or remove a package, call the tool env with the command add or remove and the manager python or tools. It changes the manifest and the lock file and installs the change. Do not run pip install, uv pip install, or apt-get for a package that the task needs again later.\n")
		default:
			b.WriteString("- This agent cannot change its packages. If one is missing, say so in your answer.\n")
		}
	}
	if inTask {
		b.WriteString("- A change outside /workspace is lost at the next suspend, and apt-get and sudo are not available to a run.\n")
		b.WriteString("- This Task has no GPU, so PyTorch and other GPU libraries run on the CPU. It has no init system and no service manager: start a server in the background of the shell instead of with systemctl.\n")
	}
	return strings.TrimSpace(b.String())
}

// manifests names the manifests for the model.
func (s *spec) manifests() string {
	var out []string
	if s.python != "" {
		out = append(out, s.rel(s.python))
	}
	if s.tools != "" {
		out = append(out, s.rel(s.tools))
	}
	if len(out) == 0 {
		return "the configuration"
	}
	return strings.Join(out, " and ")
}

// packageList renders packages as "name version", sorted as the manager
// gave them. explicitOnly keeps the packages that the manifest names,
// when the manager marks any, because a conda environment holds many
// libraries that nobody asked for. A positive max cuts the list.
func packageList(pkgs []Package, explicitOnly bool, max int) string {
	if explicitOnly {
		var named []Package
		for _, p := range pkgs {
			if p.Explicit {
				named = append(named, p)
			}
		}
		if len(named) > 0 {
			pkgs = named
		}
	}
	var out []string
	for _, p := range pkgs {
		name, version := safeWord(p.Name), safeWord(p.Version)
		if name == "" {
			continue
		}
		if max > 0 && len(out) == max {
			out = append(out, fmt.Sprintf("and %d more", len(pkgs)-max))
			break
		}
		out = append(out, strings.TrimSpace(name+" "+version))
	}
	return strings.Join(out, ", ")
}

// safeWord returns s when it holds only the characters of a package name,
// a version, or a step, and at most 64 of them, else "".
func safeWord(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._+-! ", r):
		default:
			return ""
		}
	}
	return s
}

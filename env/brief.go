package env

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/bdubs00/featherweight-kernel/ax"
	"github.com/bdubs00/featherweight-kernel/env/pkglist"
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
	if err != nil || s == nil || !s.brief || !s.on {
		return ""
	}
	return s.briefText(s.state(), toolAllowed(prof.Tools) && s.ask, ax.Detect())
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
	// Until an install is ready, the shell has the last environment that
	// installed and still exists (toolEnv), so the section names that one.
	m := st.Marker
	if st.Kind != StateReady {
		m = s.lastEnv(st.Marker)
	}
	switch {
	case st.Kind == StateFailed && m != nil:
		fmt.Fprintf(&b, "\nThe last change of the packages failed to install at the step %s, so the shell has the packages of the last install that worked.\n", safeWord(st.Failure.Step))
	case st.Kind == StateFailed:
		fmt.Fprintf(&b, "\nThe packages failed to install at the step %s, so the shell starts without them.\n", safeWord(st.Failure.Step))
	case m != nil:
		b.WriteString("\nA change of the packages installs now. Until it is ready, the shell has the packages of the last install.\n")
	case st.Kind == StatePending, st.Kind == StateMaterializing:
		fmt.Fprintf(&b, "\nThe environment is being installed from %s now. The first command of the shell waits until it is ready.\n", s.manifests())
	}
	b.WriteString("\n")
	pyList, toolsList := s.isInline(pkglist.Python), s.isInline(pkglist.Tools)
	if s.python != "" {
		switch {
		case pyList && (m == nil || m.Python == nil) && st.Kind == StateReady:
			b.WriteString("- Python: no Python packages yet, so there is no virtual environment. The `python3` of the image is on PATH.")
		case pyList:
			fmt.Fprintf(&b, "- Python: the virtual environment %s, with uv as the manager, from the list of Python packages of this agent. VIRTUAL_ENV is set, and `python` and the programs of the packages are on PATH.", s.venv())
		default:
			fmt.Fprintf(&b, "- Python: the virtual environment %s, from %s and %s, with uv as the manager. VIRTUAL_ENV is set, and `python` and the programs of the packages are on PATH.", s.venv(), s.rel(s.python), s.rel(s.uvLock()))
		}
		if m != nil && m.Python != nil {
			if v := safeWord(m.Python.Version); v != "" {
				fmt.Fprintf(&b, " Python %s.", v)
			}
			// For a list, the packages that the list names come first,
			// because a library brings many packages that nobody asked
			// for.
			if list := packageList(m.Python.Packages, pyList, briefPackages); list != "" {
				fmt.Fprintf(&b, " Packages: %s.", list)
			}
		}
		b.WriteString("\n")
	}
	if s.tools != "" {
		switch {
		case toolsList && (m == nil || m.Tools == nil) && st.Kind == StateReady:
			b.WriteString("- Tools: no programs from conda-forge yet.")
		case toolsList:
			b.WriteString("- Tools: programs and libraries from conda-forge, with pixi as the manager, from the list of programs of this agent. Their programs are on PATH.")
		default:
			fmt.Fprintf(&b, "- Tools: programs and libraries from conda-forge, from %s and %s, with pixi as the manager. Their programs are on PATH.", s.rel(s.tools), s.rel(s.pixiLock()))
		}
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
		case canChange && s.inline != nil:
			fmt.Fprintf(&b, "- To add or remove a package, call the tool env with the command add or remove and the manager python or tools. A person allows each change. The tool installs the change and keeps it after a suspend. Do not run pip install, uv pip install, or apt-get for a package that the task needs again later, and do not edit the files under %s, because fw writes them again.\n", filepath.Join(s.dir, "src"))
		case canChange:
			b.WriteString("- To add or remove a package, call the tool env with the command add or remove and the manager python or tools. It changes the manifest and the lock file and installs the change. Do not run pip install, uv pip install, or apt-get for a package that the task needs again later.\n")
		default:
			b.WriteString("- This agent cannot change its packages. If one is missing, say so in your answer.\n")
		}
	}
	if s.mirrors != nil {
		var parts []string
		if s.mirrors[pkglist.Python] != "" {
			parts = append(parts, "UV_DEFAULT_INDEX and PIP_INDEX_URL name it for Python")
		}
		if s.mirrors[pkglist.Tools] != "" {
			parts = append(parts, "pixi uses it for conda-forge")
		}
		fmt.Fprintf(&b, "- Packages come from the mirror of this deployment: %s. Other package registries can be out of reach.\n", strings.Join(parts, ", and "))
	}
	if inTask {
		b.WriteString("- A change outside /workspace is lost at the next suspend, and apt-get and sudo are not available to a run.\n")
		b.WriteString("- This Task has no GPU, so PyTorch and other GPU libraries run on the CPU. It has no init system and no service manager: start a server in the background of the shell instead of with systemctl.\n")
	}
	return strings.TrimSpace(b.String())
}

// manifests names the manifests for the model, or the lists.
func (s *spec) manifests() string {
	var out []string
	switch {
	case s.isInline(pkglist.Python):
		out = append(out, "the list of Python packages")
	case s.python != "":
		out = append(out, s.rel(s.python))
	}
	switch {
	case s.isInline(pkglist.Tools):
		out = append(out, "the list of programs")
	case s.tools != "":
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

package env

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Name is the name of the tool.
const Name = "env"

// Managers of the tool: the keys of the block that name the manifests.
const (
	ManagerPython = "python"
	ManagerTools  = "tools"
)

const toolSchema = `{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "enum": ["list", "add", "remove"],
      "description": "list shows the state, the paths, and the packages of the environment. add installs a package and records it in the manifest and the lock file. remove takes it out again."
    },
    "manager": {
      "type": "string",
      "enum": ["python", "tools"],
      "description": "python for a package from PyPI through uv; tools for a program or a library from conda-forge through pixi, such as nodejs, git, or postgresql. Needed for add and remove."
    },
    "package": {
      "type": "string",
      "description": "For add, a requirement, such as \"pyyaml>=6\" or \"nodejs>=22\". For remove, the name of the package."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`

const toolDescription = "Manage the packages of the environment of this agent. " +
	"The environment is on the durable volume, so a package stays after a suspend, which a pip install or an apt-get install does not. " +
	"list shows what is installed. add and remove change the manifest and the lock file and install the change, which can take a few minutes for a large package."

// tool is the tool env. Without the block it is opt-in, so no profile
// gets it unless its tool list names it.
type tool struct {
	cfg *fw.Config
	s   *spec
}

func newTool(cfg *fw.Config) (fw.Tool, error) {
	s, err := load(cfg)
	if err != nil {
		return nil, err
	}
	return &tool{cfg: cfg, s: s}, nil
}

// Spec implements fw.Tool.
func (t *tool) Spec() fw.ToolSpec {
	return fw.ToolSpec{
		Name:        Name,
		Description: toolDescription,
		InputSchema: json.RawMessage(toolSchema),
		Source:      "builtin",
		Trusted:     true,
		Title:       "Manage packages",
		OptIn:       t.s == nil,
	}
}

// input is the input of a call.
type input struct {
	Command string `json:"command"`
	Manager string `json:"manager"`
	Package string `json:"package"`
}

// Call implements fw.Tool.
func (t *tool) Call(ctx context.Context, raw json.RawMessage) (fw.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return fw.ErrorResult("env: the input is not valid: " + err.Error()), nil
	}
	if t.s == nil {
		return fw.ErrorResult("env: this agent has no environment; a person adds the block environment to fw.yaml."), nil
	}
	switch in.Command {
	case "list":
		return fw.TextResult(t.list()), nil
	case "add", "remove":
		return t.change(ctx, in), nil
	}
	return fw.ErrorResult(fmt.Sprintf("env: the command %q is not list, add, or remove.", in.Command)), nil
}

// list describes the environment for the model.
func (t *tool) list() string {
	s := t.s
	st := s.state()
	var b strings.Builder
	fmt.Fprintf(&b, "State: %s (digest %s).\nDirectory: %s (placement %s).\n", st.Kind, short(st.Digest), s.dir, s.placement)
	if st.Failure != nil {
		fmt.Fprintf(&b, "The install failed at the step %s: %s.\n", st.Failure.Step, st.Failure.Error)
	}
	m := st.Marker
	if s.python != "" {
		fmt.Fprintf(&b, "Python: %s and %s, virtual environment %s.\n", s.rel(s.python), s.rel(s.uvLock()), s.venv())
		if m != nil && m.Python != nil {
			fmt.Fprintf(&b, "Python packages: %s.\n", packageList(m.Python.Packages, false, 0))
		}
	}
	if s.tools != "" {
		fmt.Fprintf(&b, "Tools: %s and %s.\n", s.rel(s.tools), s.rel(s.pixiLock()))
		if m != nil && m.Tools != nil {
			fmt.Fprintf(&b, "Tool packages: %s.\n", packageList(m.Tools.Packages, true, 0))
		}
	}
	if s.python == "" && s.tools == "" {
		b.WriteString("No manifest is configured, so add and remove are not available.\n")
	}
	return strings.TrimSpace(b.String())
}

// change runs add or remove through the manager, installs the result, and
// records fw.env.changed. It holds the lock of the environment, so no
// install runs beside it.
func (t *tool) change(ctx context.Context, in input) fw.ToolResult {
	s := t.s
	if s.placement == PlacementImage {
		return fw.ErrorResult("env: the environment is part of the image (placement image). Change the manifest, then build the image again with fw env dockerfile.")
	}
	manifest := map[string]string{ManagerPython: s.python, ManagerTools: s.tools}[in.Manager]
	switch {
	case in.Manager != ManagerPython && in.Manager != ManagerTools:
		return fw.ErrorResult(fmt.Sprintf("env: the manager %q is not python or tools.", in.Manager))
	case manifest == "":
		return fw.ErrorResult(fmt.Sprintf("env: the environment has no %s manifest; a person sets environment.%s in fw.yaml.", in.Manager, in.Manager))
	}
	pkg := strings.TrimSpace(in.Package)
	if err := checkPackage(pkg); err != nil {
		return fw.ErrorResult("env: " + err.Error())
	}
	inst := newInstaller(fw.AgentFrom(ctx), t.cfg, s)
	unlock, err := inst.lock(ctx)
	if err != nil {
		return fw.ErrorResult(err.Error())
	}
	defer unlock()
	previous := s.digest()
	dir := filepath.Dir(manifest)
	var args []string
	switch in.Manager {
	case ManagerPython:
		args = []string{"uv", in.Command, "--no-sync", "--project", dir, "--", pkg}
	case ManagerTools:
		args = []string{"pixi", in.Command, "--no-install", "--manifest-path", manifest, "--", pkg}
	}
	if out, err := inst.run(ctx, inst.env(ctx, s.readMarker()), dir, args[0], args[1:]...); err != nil {
		msg := fmt.Sprintf("env: %s %s failed: %v. The manifest and the lock file are as they were.", args[0], in.Command, err)
		if out = inst.redact(out); out != "" {
			msg += " The output ends with:\n" + out
		}
		return fw.ErrorResult(msg)
	}
	digest := s.digest()
	if f := inst.install(ctx, digest, nil); f != nil {
		return fw.ErrorResult(fmt.Sprintf("env: %s %s changed %s, but the install failed. %v", args[0], in.Command, s.rel(manifest), failureError(f)))
	}
	if ctx.Err() != nil {
		return fw.ErrorResult(fmt.Sprintf("env: %s %s changed %s, but the call ended before the install finished; the next command installs it.", args[0], in.Command, s.rel(manifest)))
	}
	run := fw.RunInfoFrom(ctx)
	inst.log.Info("the environment changed", "digest", short(digest), "manager", in.Manager, "command", in.Command, "package", pkg, "run", run.RunID)
	inst.publish(EvChanged, ChangedData{Digest: digest, Previous: previous, Manager: in.Manager, Command: in.Command, Package: pkg, Run: run.RunID, Agent: run.Agent})
	verb := map[string]string{"add": "Added", "remove": "Removed"}[in.Command]
	return fw.TextResult(fmt.Sprintf("%s %s in %s and its lock file. The environment is ready (digest %s).\n%s", verb, pkg, s.rel(manifest), short(digest), t.list()))
}

// checkPackage refuses a package that a manager would read as an option,
// and control characters.
func checkPackage(pkg string) error {
	switch {
	case pkg == "":
		return fmt.Errorf("the package is empty; name a requirement such as \"pyyaml>=6\"")
	case len(pkg) > 256:
		return fmt.Errorf("the package is longer than 256 characters")
	case strings.HasPrefix(pkg, "-"):
		return fmt.Errorf("the package %q starts with a dash", pkg)
	}
	for _, r := range pkg {
		if unicode.IsControl(r) {
			return fmt.Errorf("the package holds a control character")
		}
	}
	return nil
}

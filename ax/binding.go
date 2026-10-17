package ax

import "path"

// DefaultWorkspacePath is the durable volume that ax mounts. A binding with
// no path lands at DefaultWorkspacePath/<name>, and a Task with no bindings
// gets one empty working directory here.
const DefaultWorkspacePath = "/workspace"

// Binding pairs one spec.workspaces entry with the Workspace that satisfies
// it and the directory where it is prepared.
type Binding struct {
	Ref WorkspaceRef
	// Workspace is nil when the Task names a Workspace that ax did not deliver.
	// Such a binding is prepared as an empty directory.
	Workspace *Workspace
	Path      string
}

// Bindings lines up the bindings of a Task with the delivered Workspaces,
// matched by name, in the order of spec.workspaces. The first binding is the
// working directory of spec.command. A nil Task, or a Task with no bindings,
// gets one binding at /workspace with a nil Workspace.
func Bindings(task *Task, workspaces []*Workspace) []Binding {
	byName := make(map[string]*Workspace, len(workspaces))
	for _, ws := range workspaces {
		if ws == nil {
			continue
		}
		if _, dup := byName[ws.Metadata.Name]; !dup {
			byName[ws.Metadata.Name] = ws
		}
	}
	var refs []WorkspaceRef
	if task != nil {
		refs = task.Spec.Workspaces
	}
	out := make([]Binding, 0, len(refs))
	for _, ref := range refs {
		p := ref.Path
		if p == "" {
			p = path.Join(DefaultWorkspacePath, ref.Name)
		}
		out = append(out, Binding{Ref: ref, Workspace: byName[ref.Name], Path: path.Clean(p)})
	}
	if len(out) == 0 {
		out = append(out, Binding{Path: DefaultWorkspacePath})
	}
	return out
}

// Workdir returns the working directory for spec.command: the path of the
// first binding.
func Workdir(bindings []Binding) string {
	if len(bindings) == 0 {
		return DefaultWorkspacePath
	}
	return bindings[0].Path
}

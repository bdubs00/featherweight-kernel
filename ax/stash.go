package ax

import (
	"slices"
	"strings"
	"sync"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// stash holds the manifests for the axmeta surface, which the registry
// builds from the configuration alone.
var stash struct {
	mu         sync.RWMutex
	task       *Task
	workspaces []*Workspace
}

// Stash records the manifests that the axmeta surface serves. Call it once at
// boot, before the agent serves.
func Stash(task *Task, workspaces []*Workspace) {
	stash.mu.Lock()
	defer stash.mu.Unlock()
	stash.task = task
	stash.workspaces = slices.Clone(workspaces)
}

// Stashed returns the manifests recorded by Stash. Callers must not change
// them; use Redacted for a copy that is safe to serve.
func Stashed() (*Task, []*Workspace) {
	stash.mu.RLock()
	defer stash.mu.RUnlock()
	return stash.task, slices.Clone(stash.workspaces)
}

// Redacted returns a copy of the Task in which the value of every spec.env
// entry with a secret name (SecretName) is "***". A nil Task gives nil.
func Redacted(task *Task) *Task {
	if task == nil {
		return nil
	}
	t := *task
	t.Spec.Command = slices.Clone(task.Spec.Command)
	t.Spec.Workspaces = slices.Clone(task.Spec.Workspaces)
	t.Spec.Env = slices.Clone(task.Spec.Env)
	for i, e := range t.Spec.Env {
		if e.Value != "" && SecretName(e.Name) {
			t.Spec.Env[i].Value = "***"
		}
	}
	return &t
}

// RedactedWorkspace returns a copy of the Workspace in which the URL of
// every spec.git repository has no user information, as RedactURL gives
// it, because a token in such a URL clones a private repository. A nil
// Workspace gives nil.
func RedactedWorkspace(ws *Workspace) *Workspace {
	if ws == nil {
		return nil
	}
	w := *ws
	w.Spec.Git = slices.Clone(ws.Spec.Git)
	for i, g := range w.Spec.Git {
		w.Spec.Git[i].Repo = RedactURL(g.Repo)
	}
	return &w
}

// SecretName reports whether an environment variable name looks like it
// holds a secret: it contains KEY, TOKEN, SECRET, or PASSWORD, in any case,
// or the redactor of the ledger counts it as a secret (fw.IsSecretName),
// as for DB_PASS or GH_PAT.
func SecretName(name string) bool {
	n := strings.ToUpper(name)
	for _, w := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD"} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return fw.IsSecretName(name)
}

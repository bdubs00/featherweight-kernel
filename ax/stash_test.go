package ax

import "testing"

func TestStash(t *testing.T) {
	t.Cleanup(func() { Stash(nil, nil) })
	task := &Task{Metadata: ObjectMeta{Name: "t"}}
	ws := []*Workspace{{Metadata: ObjectMeta{Name: "w"}}}
	Stash(task, ws)
	ws[0] = nil
	gt, gw := Stashed()
	if gt != task || len(gw) != 1 || gw[0] == nil || gw[0].Metadata.Name != "w" {
		t.Errorf("Stashed = %v, %v", gt, gw)
	}
}

func TestRedacted(t *testing.T) {
	task := &Task{Spec: TaskSpec{Env: []EnvVar{
		{Name: "FW_AGENT", Value: "reviewer"},
		{Name: "ANTHROPIC_API_KEY", Value: "sk-ant-1"},
		{Name: "github_token", Value: "ghp_1"},
		{Name: "Db_Password", Value: "hunter2"},
		{Name: "CLIENT_SECRET", Value: "s"},
		{Name: "EMPTY_TOKEN", Value: ""},
		{Name: "MONKEY", Value: "banana"},
	}}}
	r := Redacted(task)
	want := []string{"reviewer", "***", "***", "***", "***", "", "***"}
	for i, e := range r.Spec.Env {
		if e.Value != want[i] {
			t.Errorf("%s = %q, want %q", e.Name, e.Value, want[i])
		}
	}
	if task.Spec.Env[1].Value != "sk-ant-1" {
		t.Error("Redacted changed the original")
	}
	if Redacted(nil) != nil {
		t.Error("Redacted(nil) is not nil")
	}
}

func TestSecretNameFollowsTheRedactor(t *testing.T) {
	for name, want := range map[string]bool{"DB_PASS": true, "GH_PAT": true, "API_KEY": true, "FW_AGENT": false} {
		if got := SecretName(name); got != want {
			t.Errorf("SecretName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRedactedWorkspace(t *testing.T) {
	ws := &Workspace{Spec: WorkspaceSpec{Git: []GitRepo{
		{Name: "app", Repo: "https://x-access-token:ghp_dddddddddddddddddddddddddddddddddddd@github.com/o/app.git"},
		{Name: "open", Repo: "https://github.com/o/open.git"},
	}}}
	r := RedactedWorkspace(ws)
	if got := r.Spec.Git[0].Repo; got != "https://***@github.com/o/app.git" {
		t.Errorf("repo = %q", got)
	}
	if got := r.Spec.Git[1].Repo; got != "https://github.com/o/open.git" {
		t.Errorf("open repo = %q", got)
	}
	if ws.Spec.Git[0].Repo == r.Spec.Git[0].Repo {
		t.Error("RedactedWorkspace changed the original")
	}
	if RedactedWorkspace(nil) != nil {
		t.Error("RedactedWorkspace(nil) is not nil")
	}
}

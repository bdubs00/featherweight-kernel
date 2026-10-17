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

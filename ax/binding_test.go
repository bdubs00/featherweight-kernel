package ax

import "testing"

func TestBindings(t *testing.T) {
	task := &Task{Spec: TaskSpec{Workspaces: []WorkspaceRef{
		{Name: "code", Goal: "run the tests"},
		{Name: "tools", Path: "/workspace/tools/"},
		{Name: "missing"},
	}}}
	tools := &Workspace{Metadata: ObjectMeta{Name: "tools"}}
	code := &Workspace{Metadata: ObjectMeta{Name: "code"}}
	other := &Workspace{Metadata: ObjectMeta{Name: "other"}}
	bs := Bindings(task, []*Workspace{tools, nil, other, code})
	if len(bs) != 3 {
		t.Fatalf("got %d bindings, want 3", len(bs))
	}
	want := []struct {
		name, path string
		ws         *Workspace
	}{
		{"code", "/workspace/code", code},
		{"tools", "/workspace/tools", tools},
		{"missing", "/workspace/missing", nil},
	}
	for i, w := range want {
		b := bs[i]
		if b.Ref.Name != w.name || b.Path != w.path || b.Workspace != w.ws {
			t.Errorf("binding %d = {%q %q %p}, want {%q %q %p}", i, b.Ref.Name, b.Path, b.Workspace, w.name, w.path, w.ws)
		}
	}
	if bs[0].Ref.Goal != "run the tests" {
		t.Errorf("goal = %q", bs[0].Ref.Goal)
	}
	if got := Workdir(bs); got != "/workspace/code" {
		t.Errorf("Workdir = %q", got)
	}
}

func TestBindingsDefault(t *testing.T) {
	for _, task := range []*Task{nil, {}} {
		bs := Bindings(task, []*Workspace{{Metadata: ObjectMeta{Name: "unbound"}}})
		if len(bs) != 1 || bs[0].Path != "/workspace" || bs[0].Workspace != nil || bs[0].Ref.Name != "" {
			t.Errorf("Bindings(%v) = %+v", task, bs)
		}
	}
	if got := Workdir(nil); got != "/workspace" {
		t.Errorf("Workdir(nil) = %q", got)
	}
}

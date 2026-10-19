package ax

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
	v1alpha1 "github.com/google/ax/pkg/apis/v1alpha1"
)

// dnsLabel is the shape of a Task name.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// The name of a child Task comes from the id of its run: the same id gives
// the same name, another id another name, and every name is a DNS label of
// at most 63 characters that ends with the id, also below a long parent.
func TestChildTaskNameIsStable(t *testing.T) {
	one, two := fw.NewID(), fw.NewID()
	for _, parent := range []string{"migrator", "Migrator", strings.Repeat("a", 80), "migrator-" + strings.ToLower(fw.NewID()) + "-" + strings.ToLower(fw.NewID())} {
		a, err := childName(parent, one)
		if err != nil {
			t.Fatal(err)
		}
		again, _ := childName(parent, one)
		b, _ := childName(parent, two)
		if a != again {
			t.Fatalf("childName(%q) gave %q, then %q", parent, a, again)
		}
		if a == b {
			t.Fatalf("two children of %q got the name %q", parent, a)
		}
		for _, name := range []string{a, b} {
			if len(name) > maxTaskName || !dnsLabel.MatchString(name) {
				t.Fatalf("childName(%q) = %q, not a DNS label of at most %d characters", parent, name, maxTaskName)
			}
		}
		if !strings.HasSuffix(a, "-"+strings.ToLower(one)) {
			t.Fatalf("childName(%q) = %q does not end with the id of the run", parent, a)
		}
	}
	// A request without an id of NewID, as of a branch, gets a random name.
	if a, _ := childName("migrator", ""); !strings.HasPrefix(a, "migrator-") || len(a) != len("migrator-")+8 {
		t.Fatalf("childName without an id = %q", a)
	}
}

// Plan names the child Task that Spawn then creates, before anything
// exists, and refuses a request without an id of NewID. Two Spawn calls
// for the same child, as two processes would make them, create a Task of
// the same name and start the run with the same idempotency key, and the
// run takes the id of the request; another child gets another name and
// another key.
func TestSpawnKeepsTheNameAndTheKeyOfAChild(t *testing.T) {
	ctx := context.Background()
	parent := &fw.Run{ID: "01PARENT"}
	id := fw.NewID()
	req := fw.RunRequest{ID: id, Input: "migrate module x", Metadata: map[string]string{fw.MetaParent: parent.ID}}
	spawn := func(req fw.RunRequest, failStarts int) (string, string, []string, map[string]any) {
		t.Helper()
		ax := newFakeAX(t)
		router := newFakeRouter(t)
		router.failStarts = failStarts
		s := newSpawnerUnderTest(t, ax, router)
		planned, err := s.Plan(parent, req)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if tasks := ax.created(); len(tasks) != 0 {
			t.Fatalf("Plan created %d Tasks", len(tasks))
		}
		run, member, err := s.Spawn(ctx, parent, req)
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		if member != planned || run.ID != req.ID {
			t.Fatalf("Spawn = %+v at %q, Plan said %q", run, member, planned)
		}
		router.mu.Lock()
		defer router.mu.Unlock()
		return planned, member, append([]string(nil), router.keys...), router.body
	}
	planned, member, keys, body := spawn(req, 1)
	if planned != "ax:team/migrator-"+strings.ToLower(id) {
		t.Fatalf("Plan = %q", planned)
	}
	if len(keys) != 2 || keys[0] != keys[1] || keys[0] != startKey(parent.ID, strings.TrimPrefix(member, "ax:")) {
		t.Fatalf("idempotency keys = %v", keys)
	}
	if body["id"] != id {
		t.Fatalf("the child got the body %v", body)
	}
	_, member2, keys2, _ := spawn(req, 0)
	if member2 != member || keys2[0] != keys[0] {
		t.Fatalf("a second start of the same child got %q with %v, the first %q with %v", member2, keys2, member, keys)
	}
	other := req
	other.ID = fw.NewID()
	_, member3, keys3, _ := spawn(other, 0)
	if member3 == member || keys3[0] == keys[0] {
		t.Fatalf("another child got %q with %v, like the first", member3, keys3)
	}

	s := newSpawnerUnderTest(t, newFakeAX(t), newFakeRouter(t))
	if _, err := s.Plan(parent, fw.RunRequest{Input: "x"}); err == nil {
		t.Fatal("Plan named a child without an id")
	}
}

// Spawn refuses a child that takes its run under another id than the
// request names, and deletes its Task.
func TestSpawnRefusesARunUnderAnotherID(t *testing.T) {
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	router.ignoreID = true
	s := newSpawnerUnderTest(t, ax, router)
	req := fw.RunRequest{ID: fw.NewID(), Input: "x"}
	_, _, err := s.Spawn(context.Background(), &fw.Run{ID: "01PARENT"}, req)
	if err == nil || !strings.Contains(err.Error(), "took the run as 01CHILD") {
		t.Fatalf("Spawn = %v", err)
	}
	ax.mu.Lock()
	deleted := ax.deleted
	ax.mu.Unlock()
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v", deleted)
	}
}

// Find tells, after a restart, whether a child Task holds the child run: a
// Task that does not exist holds no run, a Task that never resumed runs
// for the look and then holds no run, a Task that ax deletes holds no run,
// and a Task with the run answers with it. A platform that does not answer cannot tell. Abandon deletes the
// Task, and a Task that is gone already is no error.
func TestFindAChildTaskAfterARestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ax := newFakeAX(t)
	router := newFakeRouter(t)
	s := newSpawnerUnderTest(t, ax, router)
	parent := &fw.Run{ID: "01PARENT"}

	// No Task: the process went away before it created one.
	req := fw.RunRequest{ID: fw.NewID(), Input: "x"}
	member, err := s.Plan(parent, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(ctx, member, req.ID); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Find without a Task = %v", err)
	}
	if err := s.Abandon(ctx, member); err != nil {
		t.Fatalf("Abandon without a Task = %v", err)
	}

	// A Task that the process created and never resumed.
	task, _ := Stashed()
	atespace, name := Identity(task)
	child, _ := childName(name, req.ID)
	if _, err := ax.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: s.childTask(task, atespace, child, parent)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Find(ctx, member, req.ID); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Find in a Task without the run = %v", err)
	}
	ax.mu.Lock()
	resumed := append([]string(nil), ax.resumed...)
	ax.mu.Unlock()
	if len(resumed) == 0 || resumed[len(resumed)-1] != key(atespace, child) {
		t.Fatalf("the Task did not run for the look: %v", resumed)
	}
	if err := s.Abandon(ctx, member); err != nil {
		t.Fatal(err)
	}
	if tasks := ax.created(); len(tasks) != 0 {
		t.Fatalf("Abandon left %d Tasks", len(tasks))
	}

	// A Task that ax deletes.
	if _, err := ax.CreateTask(ctx, &v1alpha1.CreateTaskRequest{Task: s.childTask(task, atespace, child, parent)}); err != nil {
		t.Fatal(err)
	}
	ax.mu.Lock()
	ax.tasks[key(atespace, child)].Status.Phase = v1alpha1.PhaseTerminating
	ax.mu.Unlock()
	if _, err := s.Find(ctx, member, req.ID); !errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Find in a Task that ax deletes = %v", err)
	}
	if err := s.Abandon(ctx, member); err != nil {
		t.Fatal(err)
	}

	// A Task that holds the run.
	run, member, err := s.Spawn(ctx, parent, req)
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.Find(ctx, member, req.ID)
	if err != nil || found.ID != run.ID {
		t.Fatalf("Find of the run = %+v, %v", found, err)
	}

	// A platform that does not answer cannot tell.
	s.cfg.AX.Server = "127.0.0.1:1"
	down, err := NewSpawner(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = down.Close() })
	short, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if _, err := down.Find(short, member, req.ID); err == nil || errors.Is(err, fw.ErrRunNotFound) {
		t.Fatalf("Find while the ax server does not answer = %v", err)
	}
}

// The native API of a child takes the run under the id that a trusted
// parent names, so the child run that the parent recorded is the run that
// the child holds.
func TestSpawnStartsTheRunUnderItsIDInTheChild(t *testing.T) {
	ax := newFakeAX(t)
	m := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{Content: []fw.Block{fw.Text("child done")}, StopReason: fw.StopEndTurn, Usage: fw.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	}}
	child, err := fw.New(fw.WithoutDefaults(), fw.WithAuthn(fw.TokenAuthn{Token: "fleet-secret"}),
		fw.WithModelName("anthropic/claude-opus-5-5"), fw.WithModel(m), fw.WithLedger(fw.NewMemLedger()),
		fw.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), fw.WithSurfaces(native.New()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		child.Close(ctx)
	})
	h, err := child.Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s := newSpawnerAt(t, ax, strings.TrimPrefix(srv.URL, "http://"))
	req := fw.RunRequest{ID: fw.NewID(), Input: "collect the summaries", Principal: "ada", PrincipalKind: fw.KindUser,
		Metadata: map[string]string{fw.MetaParent: "01PARENT"}}
	run, member, err := s.Spawn(context.Background(), &fw.Run{ID: "01PARENT"}, req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if run.ID != req.ID || run.Principal != "ada" {
		t.Fatalf("the child run = %+v", run)
	}
	got, err := s.Find(context.Background(), member, req.ID)
	if err != nil || got.ID != req.ID {
		t.Fatalf("Find in the child = %+v, %v", got, err)
	}
}

package fw

import (
	"strings"
	"testing"
	"time"
)

func TestBusIntakeAndEgress(t *testing.T) {
	ctx := testCtx(t)
	t.Setenv("FW_ATESPACE", "test")
	bus := NewMemBus()
	cfg := DefaultConfig()
	cfg.Bus.Intake = "fw.jobs.test"
	cfg.Bus.Egress = "fw.events"
	cfg.Bus.EgressTypes = []string{"fw.run.*"}

	all := make(chan Event, 100)
	stop, err := bus.Subscribe(ctx, "fw.events.>", func(e Event) error {
		all <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	finished := make(chan Event, 1)
	stop2, _ := bus.Subscribe(ctx, "fw.events.test.default.fw.run.finished", func(e Event) error {
		finished <- e
		return nil
	})
	defer stop2()

	base := []Option{WithoutDefaults(), WithAuthn(NoAuthn{}), WithLedger(NewMemLedger()), WithLogger(quietLogger())}
	a, err := New(append(base, WithModel(newScript(endTurn("from bus")).Model()), WithConfig(cfg), WithBus(bus))...)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(ctx)

	job := MustEvent("job", "fw.job", RunRequest{Input: "do the job", Metadata: map[string]string{"ticket": "42"}})
	if err := bus.Publish(ctx, "fw.jobs.test", job); err != nil {
		t.Fatal(err)
	}
	var fin Event
	select {
	case fin = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("no fw.run.finished on the egress subject")
	}
	r, err := a.Get(ctx, fin.Subject)
	if err != nil {
		t.Fatal(err)
	}
	if r.Origin != OriginBus || r.Metadata["idempotencyKey"] != job.ID || r.Metadata["ticket"] != "42" || r.Output != "from bus" {
		t.Fatalf("run = %+v", r)
	}
	if fin.Source != "fw://test/default/"+r.ID || fin.Seq == 0 {
		t.Fatalf("egress event = %+v", fin)
	}
	// A redelivered message returns the first run and acknowledges.
	if err := a.handleIntake(job); err != nil {
		t.Fatalf("redelivery = %v", err)
	}
	if list, _ := a.List(ctx); len(list) != 1 {
		t.Fatalf("redelivery started a second run: %d runs", len(list))
	}
	// Only the fw.run.* types pass the filter.
	for {
		select {
		case e := <-all:
			if !strings.HasPrefix(e.Type, "fw.run.") {
				t.Fatalf("egress published %s past the type filter", e.Type)
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}

func TestAgentOfSource(t *testing.T) {
	for src, want := range map[string]string{
		"fw://local/default/R1": "default",
		"fw://ns/a/b/R1":        "a/b",
		"http://x":              "unknown",
		"fw://only/two":         "unknown",
	} {
		if got := agentOfSource(src); got != want {
			t.Errorf("agentOfSource(%q) = %q, want %q", src, got, want)
		}
	}
}

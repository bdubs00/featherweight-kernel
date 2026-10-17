package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func newTestAgent(t *testing.T) *fw.Agent {
	t.Helper()
	model := fw.ModelFunc{GenerateFn: func(context.Context, *fw.Request) (*fw.Response, error) {
		return &fw.Response{}, nil
	}}
	a, err := fw.New(
		fw.WithoutDefaults(),
		fw.WithLedger(fw.NewMemLedger()),
		fw.WithBus(fw.NewMemBus()),
		fw.WithAuthn(fw.NoAuthn{}),
		fw.WithModel(model),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(context.Background()) })
	return a
}

func TestAskUser(t *testing.T) {
	tool, err := fw.NewBuiltinTool("ask_user", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var gotQ string
	var gotSchema []byte
	ctx := fw.WithAsker(context.Background(), func(_ context.Context, q string, schema []byte) ([]byte, error) {
		gotQ, gotSchema = q, schema
		return []byte(`{"answer":"blue"}`), nil
	})
	res := call(t, ctx, tool, `{"question":"Which color?"}`)
	if got := mustOK(t, res); got != `{"answer":"blue"}` || string(res.Structured) != got {
		t.Fatalf("answer: %q, structured %s", got, res.Structured)
	}
	if gotQ != "Which color?" || gotSchema != nil {
		t.Fatalf("asker saw %q, schema %s", gotQ, gotSchema)
	}
	mustOK(t, call(t, ctx, tool, `{"question":"Pick","schema":{"type":"object","properties":{"n":{"type":"integer"}}}}`))
	var schema map[string]any
	if err := json.Unmarshal(gotSchema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("schema not passed: %s", gotSchema)
	}

	mustFail(t, call(t, context.Background(), tool, `{"question":"Anyone?"}`), "cannot be asked here")

	failing := fw.WithAsker(context.Background(), func(context.Context, string, []byte) ([]byte, error) {
		return nil, errors.New("caller declined")
	})
	mustFail(t, call(t, failing, tool, `{"question":"Anyone?"}`), "caller declined")
}

func TestSend(t *testing.T) {
	a := newTestAgent(t)
	tool, err := fw.NewBuiltinTool("send", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan fw.Event, 1)
	stop, err := a.Bus().Subscribe(context.Background(), "orders.created", func(e fw.Event) error {
		got <- e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	ctx := fw.WithAgent(fw.WithRunInfo(context.Background(), fw.RunInfo{RunID: "run-1"}), a)
	if s := mustOK(t, call(t, ctx, tool, `{"subject":"orders.created","data":{"id":7}}`)); s != "sent to orders.created" {
		t.Fatalf("result %q", s)
	}
	select {
	case e := <-got:
		var data map[string]int
		if e.Type != "fw.tool.send" || e.Subject != "run-1" || e.Decode(&data) != nil || data["id"] != 7 {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event on the bus")
	}

	mustFail(t, call(t, ctx, tool, `{"subject":"orders.*","data":{}}`), "wildcards")
	mustFail(t, call(t, context.Background(), tool, `{"subject":"orders.created"}`), "no agent")
}

func TestSpawnTaskWithoutAgent(t *testing.T) {
	tool, err := fw.NewBuiltinTool("spawn_task", fw.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, call(t, context.Background(), tool, `{"input":"do something"}`), "no agent")
}

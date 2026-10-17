package fw

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type closeNote struct {
	log  *[]string
	name string
}

func (c closeNote) Close() error { *c.log = append(*c.log, c.name); return nil }

// New starts the registered services and the services of the options, in
// that order, and shutdown stops them, the last one first.
func TestServices(t *testing.T) {
	var log []string
	note := func(name string) Service {
		return func(_ context.Context, a *Agent) (io.Closer, error) {
			log = append(log, "start "+name)
			return closeNote{&log, "close " + name}, nil
		}
	}
	RegisterService(note("registered"))
	t.Cleanup(func() {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		registry.services = nil
	})
	model := newScript(endTurn("hi")).Model()
	base := []Option{WithConfig(DefaultConfig()), WithModel(model), WithLedger(NewMemLedger()), WithBus(NewMemBus()), WithAuthn(NoAuthn{})}

	// A service that returns nil has nothing to run.
	a, err := New(append(base, WithServices(note("mine"), func(context.Context, *Agent) (io.Closer, error) { return nil, nil }))...)
	if err != nil {
		t.Fatal(err)
	}
	a.Close(context.Background())
	if got := strings.Join(log, ","); got != "start registered,start mine,close mine,close registered" {
		t.Fatalf("log = %s", got)
	}

	// WithoutDefaults leaves the registered services out, and
	// WithoutServices leaves every service out.
	log = nil
	a, err = New(append(base, WithoutDefaults(), WithServices(note("mine")))...)
	if err != nil {
		t.Fatal(err)
	}
	a.Close(context.Background())
	if got := strings.Join(log, ","); got != "start mine,close mine" {
		t.Fatalf("WithoutDefaults log = %s", got)
	}
	log = nil
	a, err = New(append(base, WithoutServices(), WithServices(note("mine")))...)
	if err != nil {
		t.Fatal(err)
	}
	a.Close(context.Background())
	if len(log) != 0 {
		t.Fatalf("WithoutServices log = %v", log)
	}

	// A service that fails stops New, and the services before it close.
	log = nil
	_, err = New(append(base, WithoutDefaults(), WithServices(note("first"), func(context.Context, *Agent) (io.Closer, error) { return nil, errors.New("no bus") }))...)
	if err == nil || !strings.Contains(err.Error(), "no bus") {
		t.Fatalf("a failing service = %v", err)
	}
	if got := strings.Join(log, ","); got != "start first,close first" {
		t.Fatalf("log after the failure = %s", got)
	}
}

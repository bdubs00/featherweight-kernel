package nats

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// A bus whose server is down at boot opens, answers Subscribe, Consume, and
// Publish at once instead of waiting for a request timeout, and works when
// the server comes up.
func TestOpenWhileTheServerIsDown(t *testing.T) {
	if os.Getenv("NATS_URL") != "" {
		t.Skip("the test starts its own server on a port that is closed first")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cfg := fw.DefaultConfig()
	cfg.Bus = fw.BusConfig{Driver: "nats", URL: fmt.Sprintf("nats://127.0.0.1:%d", port), Intake: "down.jobs", Egress: "down.events", Options: map[string]string{"stream": "DOWN"}}

	start := time.Now()
	b, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open with the server down: %v", err)
	}
	defer b.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Open took %s with the server down", d)
	}
	start = time.Now()
	stop, err := b.Subscribe(ctx, "down.notice", func(fw.Event) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe with the server down: %v", err)
	}
	stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Subscribe took %s with the server down", d)
	}
	start = time.Now()
	if _, err := b.Consume(ctx, "down.jobs", func(fw.Event) error { return nil }); !errors.Is(err, errNotConnected) {
		t.Fatalf("Consume with the server down = %v; want errNotConnected", err)
	}
	_ = b.Publish(ctx, "down.events.x", fw.Event{ID: "1", Type: "t"})
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Consume and Publish took %s with the server down", d)
	}

	runServer(t, t.TempDir(), port)
	deadline := time.Now().Add(20 * time.Second)
	for !b.nc.IsConnected() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !b.nc.IsConnected() {
		t.Fatal("the bus did not connect after the server came up")
	}
	got := make(chan fw.Event, 1)
	var stop2 func()
	for time.Now().Before(deadline) {
		stop2, err = b.Consume(ctx, "down.jobs", func(e fw.Event) error { got <- e; return nil })
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Consume after the server came up: %v", err)
	}
	defer stop2()
	if err := b.Publish(ctx, "down.jobs", fw.Event{ID: "3", Type: "fw.job"}); err != nil {
		t.Fatalf("Publish after the server came up: %v", err)
	}
	select {
	case e := <-got:
		if e.ID != "3" {
			t.Fatalf("got %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the consumer got nothing after the server came up")
	}
}

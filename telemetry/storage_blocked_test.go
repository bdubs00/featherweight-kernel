package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/prometheus/client_golang/prometheus"
)

// failingLedger fails every append after fail is set, as a full disk does.
type failingLedger struct {
	fw.Ledger
	fail atomic.Bool
}

func (l *failingLedger) Append(ctx context.Context, e fw.Event) (uint64, error) {
	if l.fail.Load() {
		return 0, errors.New("injected disk failure")
	}
	return l.Ledger.Append(ctx, e)
}

func TestStorageBlockedGauge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l := &failingLedger{Ledger: fw.NewMemLedger()}
	a := newAgent(t, (&script{}).model(), fw.WithLedger(l))
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg, nil)
	if got := value(t, reg, "fw_storage_blocked", nil); got != 0 {
		t.Fatalf("before Attach = %v", got)
	}
	m.Attach(a)
	if got := value(t, reg, "fw_storage_blocked", nil); got != 0 {
		t.Fatalf("healthy = %v", got)
	}
	l.fail.Store(true)
	if _, err := a.Start(ctx, "work"); !errors.Is(err, fw.ErrStorageUnavailable) {
		t.Fatalf("start = %v", err)
	}
	if a.RuntimeError() == nil {
		t.Fatal("no fail-stop")
	}
	if got := value(t, reg, "fw_storage_blocked", nil); got != 1 {
		t.Fatalf("after a storage failure = %v", got)
	}
}

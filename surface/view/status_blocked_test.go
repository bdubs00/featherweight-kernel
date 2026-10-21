package view

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// cutLedger fails every append after the cut, as a full disk does.
type cutLedger struct {
	fw.Ledger
	cut atomic.Bool
}

func (l *cutLedger) Append(ctx context.Context, e fw.Event) (uint64, error) {
	if l.cut.Load() {
		return 0, errors.New("write ledger.jsonl: no space left on device")
	}
	return l.Ledger.Append(ctx, e)
}

// A process that a storage failure blocked reports itself blocked and idle
// since the failure, although its run still shows its last recorded state,
// so a controller suspends it and the next request starts a fresh process.
func TestStatusOfABlockedProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l := &cutLedger{Ledger: fw.NewMemLedger()}
	working := fw.ModelFunc{GenerateFn: func(ctx context.Context, _ *fw.Request) (*fw.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	a := newAgent(t, fw.WithLedger(l), fw.WithModel(working))
	r, err := a.Start(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitFor(ctx, r.ID, func(r *fw.Run) bool { return r.State == fw.StateRunning }); err != nil {
		t.Fatal(err)
	}
	st, err := StatusOf(ctx, a)
	if err != nil || st.Idle || st.Blocked || st.BlockedReason != "" {
		t.Fatalf("status of a working process = %+v, %v", st, err)
	}
	l.cut.Store(true)
	if _, err := a.Start(ctx, "trigger"); !errors.Is(err, fw.ErrStorageUnavailable) {
		t.Fatalf("start after the cut = %v", err)
	}
	st, err = StatusOf(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	since := a.RuntimeErrorSince()
	if !st.Blocked || !strings.Contains(st.BlockedReason, "storage unavailable") || !st.Idle || st.IdleSince == nil || since.IsZero() || !st.IdleSince.Equal(since) {
		t.Fatalf("status of a blocked process = %+v (failure at %v)", st, since)
	}
	if got, _ := a.Get(ctx, r.ID); got == nil || got.State.Terminal() || got.Blocked == "" {
		t.Fatalf("the run of the blocked process = %+v", got)
	}
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `"blocked":true`) || !strings.Contains(string(raw), `"blockedReason":`) {
		t.Fatalf("status body = %s", raw)
	}
}

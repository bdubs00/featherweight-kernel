package fw

import "testing"

func TestFileLedgerControlDurability(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"queue", MustEvent("R", EvRunQueued, RunQueuedData{Origin: OriginHTTP})},
		{"message", MustEvent("R", EvMessageReceived, MessageReceivedData{Origin: OriginHTTP})},
		{"unknown message origin", MustEvent("R", EvMessageReceived, nil)},
		{"budget", MustEvent("R", EvBudgetChanged, BudgetChangedData{})},
		{"annotation", MustEvent("R", EvRunAnnotated, RunAnnotatedData{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := OpenFileLedger(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			ctx := testCtx(t)
			// Establish the index and leave the first event batched.
			if _, err := l.Append(ctx, MustEvent("R", EvRunStarted, nil)); err != nil {
				t.Fatal(err)
			}
			l.failFileSyncs.Store(1)
			if _, err := l.Append(ctx, tc.event); err == nil {
				t.Fatal("acknowledged a failed control-event sync")
			}
			// The failed append can still exist. A later durable boundary pays
			// its sync debt; the caller cannot treat the first error as absence.
			if seq, err := l.Append(ctx, tc.event); err != nil || seq != 3 {
				t.Fatalf("retry = %d, %v", seq, err)
			}
			if l.syncs.Load() != 1 || l.dsyncs.Load() != 1 {
				t.Fatal("control-event success omitted durability debts")
			}
		})
	}
}

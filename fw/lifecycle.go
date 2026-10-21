package fw

import (
	"context"
	"fmt"
)

// SaturationError reports the configured direct admission capacity.
type SaturationError struct {
	MaxRuns       int
	MaxQueuedRuns int
}

func (e *SaturationError) Error() string {
	return fmt.Sprintf("%v (maxRuns=%d, maxQueuedRuns=%d)", ErrSaturated, e.MaxRuns, e.MaxQueuedRuns)
}

func (e *SaturationError) Unwrap() error { return ErrSaturated }

// RuntimeError returns the first storage failure of this process. A non-nil
// error means execution and writes are blocked until restart. Durable run
// states stay unchanged because a failed append can have reached storage.
func (a *Agent) RuntimeError() error {
	if a.k == nil {
		return nil
	}
	return a.k.storageError()
}

func (k *kernel) storageError() error {
	k.lifeMu.Lock()
	defer k.lifeMu.Unlock()
	return k.storageErr
}

func (k *kernel) writeErrorLocked() error {
	if k.storageErr != nil {
		return k.storageErr
	}
	if k.draining {
		return ErrClosed
	}
	return nil
}

func (k *kernel) writeError() error {
	k.lifeMu.Lock()
	defer k.lifeMu.Unlock()
	return k.writeErrorLocked()
}

// failStorage never writes a compensating event or retries an ambiguous
// append. Restart rebuilds from the ledger and applies normal tool recovery.
func (a *Agent) failStorage(cause error) error {
	k := a.k
	k.lifeMu.Lock()
	first := k.storageErr == nil
	if first {
		k.storageErr = fmt.Errorf("%w: %w", ErrStorageUnavailable, cause)
		close(k.failedCh)
	}
	err := k.storageErr
	k.lifeMu.Unlock()
	if first {
		a.log.Error("storage failure; execution blocked until restart", "error", a.Redact(err.Error()))
		k.cancel()
	}
	return err
}

func (rs *runState) accepted(ctx context.Context) (*Run, error) {
	if rs.acceptDone != nil {
		select {
		case <-rs.acceptDone:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if rs.acceptErr != nil {
			return nil, rs.acceptErr
		}
	}
	return rs.snapshot(), nil
}

func (a *Agent) reserveAdmission(rs *runState) error {
	if rs.depth != 0 || rs.standIn {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active || rs.reserved {
		return nil
	}
	return a.claimAdmissionLocked(rs)
}

// reserveAdmissionFor reserves a place for a driver that a caller may start
// while it works, even when one is active now: the driver can idle between
// the reservation and the caller's writes, and those writes then need the
// place. The caller launches into the place or releases it.
func (a *Agent) reserveAdmissionFor(rs *runState) error {
	if rs.depth != 0 || rs.standIn {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.reserved {
		return nil
	}
	return a.claimAdmissionLocked(rs)
}

// claimAdmissionLocked takes a place in the direct backlog for rs. The
// caller holds rs.mu.
func (a *Agent) claimAdmissionLocked(rs *runState) error {
	k := a.k
	k.slotMu.Lock()
	defer k.slotMu.Unlock()
	queued := a.cfg.MaxQueuedRuns
	if queued == 0 {
		queued = defaultMaxQueuedRuns
	}
	// A gated bus admission already occupies a place until its handler
	// returns. Do not count that place twice for the run it starts.
	admitted := k.admitted
	if rs.queued.Origin == OriginBus && admitted > 0 {
		admitted--
	}
	if k.rootDrivers+admitted >= cap(k.sem)+queued {
		return &SaturationError{MaxRuns: cap(k.sem), MaxQueuedRuns: queued}
	}
	rs.reserved = true
	k.rootDrivers++
	k.slotsChangedLocked()
	return nil
}

func (a *Agent) releaseAdmission(rs *runState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.reserved {
		rs.reserved = false
		a.k.addRootDriver(-1)
	}
}

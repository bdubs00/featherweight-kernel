package fw

import (
	"context"
	"errors"
	"fmt"
	"time"
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

// RuntimeErrorSince returns the time of the first storage failure of this
// process, from the clock of the agent, or the zero time when storage did
// not fail. GET /v1/status reports the process idle since that time, so a
// controller can suspend the blocked process, and the next request starts
// a fresh process that rebuilds from the ledger.
func (a *Agent) RuntimeErrorSince() time.Time {
	if a.k == nil {
		return time.Time{}
	}
	a.k.lifeMu.Lock()
	defer a.k.lifeMu.Unlock()
	return a.k.storageAt
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

// storageFault marks a failure of the ledger itself: an Append, a Flush, or
// a read of the ledger that failed, or an append after Close. Only a storage
// fault fail-stops the process (failStorage). Any other error of a driver,
// such as a broken pipe of a hosted harness or an error of a runner, fails
// the run of that driver alone.
type storageFault struct{ err error }

func (f *storageFault) Error() string { return f.err.Error() }
func (f *storageFault) Unwrap() error { return f.err }

// storageFailed marks err, an error of a ledger call, as a storage fault.
func storageFailed(err error) error {
	if err == nil {
		return nil
	}
	return &storageFault{err: err}
}

// isStorageFault reports whether err is or wraps a storage fault.
func isStorageFault(err error) bool {
	var f *storageFault
	return errors.As(err, &f)
}

// failStorage fail-stops the process on a storage fault. It never writes a
// compensating event or retries an ambiguous append. Restart rebuilds from
// the ledger and applies normal tool recovery. An error that is not a
// storage fault changes nothing, and failStorage returns it as it is.
func (a *Agent) failStorage(cause error) error {
	if !isStorageFault(cause) {
		return cause
	}
	k := a.k
	k.lifeMu.Lock()
	first := k.storageErr == nil
	if first {
		k.storageErr = fmt.Errorf("%w: %w", ErrStorageUnavailable, cause)
		k.storageAt = a.now().UTC()
		close(k.failedCh)
	}
	err, at := k.storageErr, k.storageAt
	k.lifeMu.Unlock()
	if first {
		a.log.Error("storage failure; execution blocked until restart", "error", a.Redact(err.Error()), "since", at)
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

// maxQueued returns the bound of admitted root drivers behind maxRuns:
// Config.MaxQueuedRuns, or defaultMaxQueuedRuns for zero.
func (a *Agent) maxQueued() int {
	if n := a.cfg.MaxQueuedRuns; n > 0 {
		return n
	}
	return defaultMaxQueuedRuns
}

// reserveAdmission reserves a place for a driver of rs that an event can
// start. A new start or a branch is new work and is bounded by maxRuns +
// maxQueuedRuns (bounded). A decision or a follow-up continues a run that
// this process already accepted: it takes a place for its driver, which
// waits for a working slot, but no bound refuses it.
func (a *Agent) reserveAdmission(rs *runState, bounded bool) error {
	if rs.depth != 0 || rs.standIn {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active || rs.reserved {
		return nil
	}
	return a.claimAdmissionLocked(rs, bounded)
}

// reserveAdmissionFor reserves a place for a driver that a caller may start
// while it works, even when one is active now: the driver can idle between
// the reservation and the caller's writes, and those writes then need the
// place. The caller launches into the place or releases it. A follow-up
// continues an accepted run, so the place is never refused.
func (a *Agent) reserveAdmissionFor(rs *runState) error {
	if rs.depth != 0 || rs.standIn {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.reserved {
		return nil
	}
	return a.claimAdmissionLocked(rs, false)
}

// claimAdmissionLocked takes a place in the direct backlog for rs. The
// caller holds rs.mu.
func (a *Agent) claimAdmissionLocked(rs *runState, bounded bool) error {
	if err := a.claimPlace(nil, bounded); err != nil {
		return err
	}
	rs.reserved = true
	return nil
}

// claimPlace takes a place in the direct backlog: one more admitted root
// driver. With the admission of a job that the bus intake took (t), the
// place is the one that the intake reserved for the job, so the job is
// never refused for saturation. Without one, a bounded claim fails with a
// SaturationError when maxRuns + maxQueuedRuns places are taken, the
// admissions of the intake included. The caller hands the place to a run
// (runState.reserved) or gives it back with addRootDriver(-1).
func (a *Agent) claimPlace(t *admission, bounded bool) error {
	k := a.k
	k.slotMu.Lock()
	defer k.slotMu.Unlock()
	switch {
	case t != nil && t.k == k && !t.used && !t.done:
		t.used = true
		k.admitted = max(0, k.admitted-1)
	case bounded:
		if k.rootDrivers+k.admitted >= cap(k.sem)+a.maxQueued() {
			return &SaturationError{MaxRuns: cap(k.sem), MaxQueuedRuns: a.maxQueued()}
		}
	}
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

// admission is a place in the direct backlog that the bus intake holds for
// one job, from the moment it takes the job until the handler of the job
// returns (intake.go). The start of the job takes the place over
// (claimPlace), so a job that the intake took never meets a full backlog.
// The fields are guarded by kernel.slotMu.
type admission struct {
	k    *kernel
	used bool // a start took the place over
	done bool // the place went back
}

// release gives the place back when no start took it over, for example
// when the job returned a run that a start under its key began before.
func (t *admission) release() {
	if t == nil {
		return
	}
	k := t.k
	k.slotMu.Lock()
	defer k.slotMu.Unlock()
	if t.used || t.done {
		return
	}
	t.done = true
	k.admitted = max(0, k.admitted-1)
	k.slotsChangedLocked()
}

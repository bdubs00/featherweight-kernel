package env

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The bus can refuse an event of the environment. A golden boot has no
// network yet, and a Task that restores from its snapshot goes on as the
// same process, so an event that the golden boot lost never comes again.
// A refused event therefore waits in the file .fw-env-outbox.json under
// dir, and the service sends it later, for the life of the process, with
// a pause that grows after each refusal:
//
//   - fw.env.ready and fw.env.failed wait as a flag. A retry sends the
//     state of that moment, so the newest state wins.
//   - fw.env.changed waits as it is, in order, because the front writes
//     each change into the configured lists. These events go before the
//     state.
//
// The file lives under dir, so a process that ends with a refused event,
// such as fw run, leaves it to the next process. Every change of the file
// takes its own lock file, never the lock of the installer, so a sender
// that holds that lock can record a refusal too.

const (
	outboxFile = ".fw-env-outbox.json"
	outboxLock = ".fw-env-outbox.lock"
	// outboxMax is the largest number of fw.env.changed that wait. A
	// longer queue loses its oldest event.
	outboxMax = 100
	// outboxWait bounds the wait for the lock of the outbox.
	outboxWait = 10 * time.Second
	// retryFirst is the pause after the first refusal. It doubles after
	// each refusal in a row, up to retryMax.
	retryFirst = time.Second
	retryMax   = 30 * time.Second
	// notifyIdle is how often the service looks for an event that another
	// installer of this process left, such as the tool env.
	notifyIdle = 10 * time.Second
)

// outbox is the file .fw-env-outbox.json: the events that wait for the bus.
type outbox struct {
	// State asks for the current state, because the bus refused a state
	// event. StateAt is the time of that refusal.
	State   bool      `json:"state,omitempty"`
	StateAt time.Time `json:"stateAt,omitzero"`
	// Changed holds the refused fw.env.changed events, oldest first.
	Changed []fw.Event `json:"changed,omitempty"`
}

func (o *outbox) empty() bool { return !o.State && len(o.Changed) == 0 }

func (s *spec) outboxPath() string     { return filepath.Join(s.dir, outboxFile) }
func (s *spec) outboxLockPath() string { return filepath.Join(s.dir, outboxLock) }

// withOutbox changes the outbox under its lock: it reads the file, calls
// f, and writes the result, or removes the file when nothing waits. A
// file that fw does not own counts as empty while a tool user exists.
func (s *spec) withOutbox(f func(*outbox)) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(outboxWait)
	for {
		unlock, ok, err := tryLock(s.outboxLockPath())
		if err != nil {
			return err
		}
		if ok {
			defer unlock()
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the lock %s stays taken", s.outboxLockPath())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var o outbox
	s.readOwned(s.outboxPath(), &o)
	f(&o)
	if o.empty() {
		return removeFile(s.outboxPath())
	}
	return writeFile(s.outboxPath(), &o)
}

// send publishes one event of the environment on the bus.
func (in *installer) send(e fw.Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), publishFor)
	defer cancel()
	return in.a.Bus().Publish(ctx, Subject, e)
}

// keep records an event that the bus refused, so the service sends it
// later, and wakes the service of this installer.
func (in *installer) keep(e fw.Event) {
	err := in.s.withOutbox(func(o *outbox) {
		if e.Type != EvChanged {
			o.State, o.StateAt = true, time.Now().UTC()
			return
		}
		o.Changed = append(o.Changed, e)
		if n := len(o.Changed) - outboxMax; n > 0 {
			o.Changed = append([]fw.Event(nil), o.Changed[n:]...)
			in.log.Warn("env: the events that wait for the bus are too many; the oldest go", "dropped", n, "max", outboxMax)
		}
	})
	if err != nil {
		in.log.Warn("env: keep an event for the bus", "type", e.Type, "error", err)
	}
	in.poke()
}

// poke wakes the notifier of the service, when this installer is the one
// of the service.
func (in *installer) poke() {
	if in.wake == nil {
		return
	}
	select {
	case in.wake <- struct{}{}:
	default:
	}
}

// settled clears the flag of the state after the bus took a state event
// whose send started at start. A flag that a later refusal set stays.
func (in *installer) settled(start time.Time) {
	if _, err := os.Lstat(in.s.outboxPath()); err != nil {
		return
	}
	err := in.s.withOutbox(func(o *outbox) {
		if o.State && !o.StateAt.After(start) {
			o.State, o.StateAt = false, time.Time{}
		}
	})
	if err != nil {
		in.log.Warn("env: clear the state that waits for the bus", "error", err)
	}
}

// stateEvent returns the event of the current state: fw.env.ready with
// the marker, or fw.env.failed with the failure. A state that installs
// now, or waits for its install, has no event, because its install sends
// the result.
func (in *installer) stateEvent(st state) (fw.Event, bool) {
	var typ string
	var data any
	switch {
	case st.Kind == StateReady && st.Marker != nil:
		typ, data = EvReady, ReadyData{Marker: st.Marker, Switch: in.s.switchOf()}
	case st.Kind == StateFailed && st.Failure != nil:
		typ, data = EvFailed, FailedData{Failure: st.Failure, Switch: in.s.switchOf()}
	default:
		return fw.Event{}, false
	}
	e, err := fw.NewEvent("", typ, data)
	if err != nil {
		in.log.Warn("env: event", "type", typ, "error", err)
		return fw.Event{}, false
	}
	e.Node = in.a.Node()
	return e, true
}

// errNotYet says that the state that waits has no event yet, because an
// install runs or waits to run. Its result follows soon.
var errNotYet = errors.New("the state installs now")

// owe records at the start of the service that the front needs the
// current state, as a flag in the outbox. An install that sends a newer
// state clears the flag, so the start sends no copy of it. owe reports
// whether the outbox holds the flag.
func (in *installer) owe() bool {
	err := in.s.withOutbox(func(o *outbox) {
		if !o.State {
			o.State, o.StateAt = true, time.Now().UTC()
		}
	})
	if err != nil {
		in.log.Warn("env: record the start event in the outbox", "error", err)
	}
	return err == nil
}

// flush sends what waits in the outbox: each fw.env.changed in order, then
// the current state when the flag asks for it or owed is true. owed is
// the start event when the outbox cannot hold the flag. It reports
// whether the start still owes the state, and the refusal of the bus,
// or errNotYet while the state installs. What the bus did not take
// waits on.
func (in *installer) flush(owed bool) (bool, error) {
	in.reload()
	if _, err := os.Lstat(in.s.outboxPath()); err != nil && !owed {
		return false, nil
	}
	var refused error
	err := in.s.withOutbox(func(o *outbox) {
		for len(o.Changed) > 0 {
			if err := in.send(o.Changed[0]); err != nil {
				refused = err
				return
			}
			in.log.Info("env: the bus took an event that waited", "type", o.Changed[0].Type, "id", o.Changed[0].ID)
			o.Changed = o.Changed[1:]
		}
		if !o.State && !owed {
			return
		}
		e, ok := in.stateEvent(in.s.state())
		if !ok {
			refused = errNotYet
			return
		}
		if err := in.send(e); err != nil {
			refused = err
			if !o.State {
				o.State, o.StateAt = true, time.Now().UTC()
			}
			owed = false
			return
		}
		in.log.Info("env: the bus took the state", "type", e.Type)
		o.State, o.StateAt, owed = false, time.Time{}, false
	})
	if err != nil {
		return owed, err
	}
	return owed, refused
}

// notify sends what waits in the outbox for the life of the process, the
// state that the start finds first, until ctx ends. After a refusal, it
// waits retryFirst, and the pause doubles after each refusal in a row, up
// to retryMax. While the state installs, it looks again after retryFirst.
// Otherwise it looks again every notifyIdle, or at once when the
// installer of the service kept an event.
func (in *installer) notify(ctx context.Context) {
	if in.a == nil || in.a.Bus() == nil {
		return
	}
	owed := !in.owe()
	pause := retryFirst
	for {
		var err error
		owed, err = in.flush(owed)
		wait := notifyIdle
		switch {
		case errors.Is(err, errNotYet):
			wait = retryFirst
		case err != nil:
			in.log.Debug("env: the bus refused an event that waits", "error", err, "retryIn", pause)
			wait = pause
			pause = min(pause*2, retryMax)
		default:
			pause = retryFirst
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-in.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

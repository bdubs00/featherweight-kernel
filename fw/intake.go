package fw

// This file is the bus intake (docs/ARCHITECTURE.md section 9.3). The
// intake consumes run requests from the queue Config.Bus.Intake. It takes
// a message only when a run slot is free, acknowledges the message as soon
// as the run is in the ledger, and does not wait for the end of the run.
// When the run of a job fails, the kernel puts the job on the queue again
// up to Config.Bus.IntakeRetries times, and then sends a record to the
// dead-letter subject "<intake>.dead". A request that cannot start goes to
// the dead letter at once.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"sync"
	"time"
)

// intakeRetryMax caps the pause between two attempts to start the intake,
// and between two attempts to send what the intake owes after a failure.
const intakeRetryMax = 30 * time.Second

// Metadata keys of a run from the bus intake.
const (
	// MetaIntakeQueue names the intake queue of a run from the bus. The
	// kernel sets it when it starts the run of a message. Only a run with
	// it gets a retry or a dead-letter record.
	MetaIntakeQueue = "fw.intake.queue"
	// MetaIntakeAttempt is the attempt of the job that the run is: none or
	// "1" for the first, "2" for the first retry.
	MetaIntakeAttempt = "fw.intake.attempt"
	// MetaIntakeRetryOf names the failed run that a retry repeats.
	MetaIntakeRetryOf = "fw.intake.retryOf"
	// MetaIntakeOutcome records what the kernel did after the run of a
	// job failed: IntakeRetried, IntakeDeadLettered, or IntakeDropped. A
	// failed run without it still owes that step, also after a restart.
	MetaIntakeOutcome = "fw.intake.outcome"
)

// Values of MetaIntakeOutcome.
const (
	IntakeRetried      = "retried"       // the job went on the queue again as a new run
	IntakeDeadLettered = "dead-lettered" // the dead-letter subject got a record of the job
	IntakeDropped      = "dropped"       // the last attempt failed, and the dead letter is off
)

// EvRunRequest is the type of a bus message that carries a RunRequest for
// the intake, such as a retry that the kernel sends. The intake takes a
// run request of any type.
const EvRunRequest = "fw.run.request"

// EvIntakeDead is the type of a dead-letter record of the bus intake. It
// is not a ledger event. The data is IntakeDeadData.
const EvIntakeDead = "fw.intake.dead"

// Outcomes of a dead-letter record.
const (
	DeadFailed   = "failed"   // the run of the last attempt failed
	DeadRejected = "rejected" // the request did not start a run
)

// IntakeDeadData is the payload of fw.intake.dead, the record that the
// dead-letter subject "<intake>.dead" receives for a job that did not end
// well. Queue is the intake queue. Request is the run request: for a
// failed run, the request as the run recorded it, with references in place
// of the bytes of its files; for a rejected request, the request as the
// message carried it, without the bytes of its files, or the data of the
// message when it is not a run request. Run is the id of the failed run,
// and Message is the id of the bus message of a rejected request. Error
// says why. Attempts counts the attempts of the job, this one included.
// Node names the process that sent the record (Config.Node).
type IntakeDeadData struct {
	Queue    string          `json:"queue"`
	Request  json.RawMessage `json:"request,omitempty"`
	Run      string          `json:"run,omitempty"`
	Message  string          `json:"message,omitempty"`
	Outcome  string          `json:"outcome"`
	Error    string          `json:"error,omitempty"`
	Attempts int             `json:"attempts"`
	Node     string          `json:"node,omitempty"`
}

// startIntake consumes Config.Bus.Intake. Each message is a RunRequest in
// JSON. The message id is the idempotency key when the request has none, so
// a message that comes back to this process returns the first run. The
// handler acknowledges the message when fw.run.queued of the run is in the
// ledger. With a GatedBus, the consumer takes a message only for a free run
// slot (intakeGate).
//
// The bus can be out of reach when the process boots. In a sandbox the
// egress policy can take effect seconds after the first packet, and a bus
// server can restart. A consumer that does not start is a warning, not a
// failure: the kernel tries again in the background, with a pause that
// doubles up to intakeRetryMax, until the consumer starts or the kernel
// closes. The process serves its API and its health in the meantime.
func (a *Agent) startIntake() {
	queue := a.cfg.Bus.Intake
	if queue == "" || a.bus == nil {
		return
	}
	if !exactSubject(queue) && (a.cfg.Bus.IntakeRetries > 0 || a.cfg.Bus.DeadLetterOn()) {
		a.log.Warn("bus intake: the queue has a wildcard, so the kernel can send no retry and no dead-letter record", "queue", queue)
	}
	if a.tryIntake(queue) {
		return
	}
	a.k.wg.Add(1)
	go func() {
		defer a.k.wg.Done()
		delay := time.Second
		for {
			t := time.NewTimer(delay)
			select {
			case <-a.k.ctx.Done():
				t.Stop()
				return
			case <-a.k.drainCh:
				t.Stop()
				return
			case <-t.C:
			}
			if a.tryIntake(queue) {
				return
			}
			delay = min(delay*2, intakeRetryMax)
		}
	}()
}

// tryIntake starts the consumer once. It returns true when the consumer
// runs, or when the kernel is closing and a retry has no point.
func (a *Agent) tryIntake(queue string) bool {
	k := a.k
	var stop func()
	var err error
	if gb, ok := a.bus.(GatedBus); ok {
		// Each message holds the admission that the gate gave it until its
		// handler returns. The start of its run takes that admission over
		// as its place in the backlog.
		stop, err = gb.ConsumeGated(k.ctx, queue, intakeGate{k: k}, func(e Event) error {
			t := &admission{k: k}
			defer t.release()
			return a.handleJob(queue, e, t)
		})
	} else {
		stop, err = a.bus.Consume(k.ctx, queue, func(e Event) error { return a.handleIntake(queue, e) })
	}
	if err != nil {
		if k.ctx.Err() != nil {
			return true
		}
		a.log.Warn("bus intake not started yet; retrying in the background", "queue", queue, "error", err)
		return false
	}
	k.lifeMu.Lock()
	if k.draining {
		k.lifeMu.Unlock()
		stop()
		return true
	}
	k.intakeStop = stop
	k.lifeMu.Unlock()
	a.log.Info("bus intake started", "queue", queue)
	return true
}

// handleIntake starts the run of one message of the queue. It returns nil
// to acknowledge the message once fw.run.queued of the run is in the
// ledger: the file ledger syncs that event of a run from the bus before
// Append returns (needsIntentSync). From then on the ledger holds the job.
// A restart resumes the run from the ledger, and the bus does not deliver
// the message again. The handler never waits for the run, so a run that
// waits for a person holds no message, and a shutdown waits for no run.
//
// The handler acknowledges a request that cannot start after it sent the
// record to the dead letter, because the bus delivers it again and again
// until the handler does. A message that arrives while the kernel closes
// returns ErrClosed, so the bus delivers it again, to this process or to
// another one.
//
// handleIntake handles a message of a bus without a gate: it first waits
// for a place in the backlog (backlogPlace), so a full backlog never
// refuses the job and never makes a bus drop it after its redeliveries.
func (a *Agent) handleIntake(queue string, e Event) error {
	return a.handleJob(queue, e, nil)
}

// handleJob is handleIntake for a message that holds the admission t of
// the gate, or nil for a bus without a gate.
func (a *Agent) handleJob(queue string, e Event, t *admission) error {
	a.countBus("in")
	var req RunRequest
	if err := json.Unmarshal(e.Data, &req); err != nil {
		return a.rejectJob(queue, e, nil, fmt.Errorf("fw: bus intake: the message is not a run request: %w", err))
	}
	if t == nil {
		var err error
		if t, err = a.backlogPlace(a.k.ctx); err != nil {
			return err
		}
		defer t.release()
	}
	sent := req
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = e.ID
	}
	req.Origin = OriginBus
	req.Metadata = maps.Clone(req.Metadata)
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	// The kernel records the queue and the outcome itself. An outcome in a
	// request makes the kernel skip what a failure owes.
	delete(req.Metadata, MetaIntakeOutcome)
	req.Metadata[MetaIntakeQueue] = queue
	if _, err := a.startJob(a.k.ctx, req, t); err != nil {
		if temporaryIntakeError(err) {
			return err
		}
		return a.rejectJob(queue, e, &sent, err)
	}
	return nil
}

// temporaryIntakeError reports whether a job that did not start goes back
// to the bus for another delivery instead of to the dead letter: the drain,
// a storage failure, a full backlog, and the end of a context, such as the
// kernel context at a shutdown or a fail-stop. The job itself is valid.
func temporaryIntakeError(err error) bool {
	return errors.Is(err, ErrClosed) || errors.Is(err, ErrSaturated) || errors.Is(err, ErrStorageUnavailable) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// backlogPlace waits for a place in the backlog of maxRuns +
// maxQueuedRuns for a job of a bus without a gate, and takes it as the
// admission of the job. It returns ErrClosed at the drain, the storage
// failure, or the error of ctx.
func (a *Agent) backlogPlace(ctx context.Context) (*admission, error) {
	k := a.k
	for {
		if err := k.writeError(); err != nil {
			return nil, err
		}
		k.slotMu.Lock()
		if k.rootDrivers+k.admitted < cap(k.sem)+a.maxQueued() {
			k.admitted++
			k.slotMu.Unlock()
			return &admission{k: k}, nil
		}
		changed := k.slotsChanged
		k.slotMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-k.drainCh:
			return nil, ErrClosed
		case <-k.failedCh:
			return nil, k.storageError()
		}
	}
}

// rejectJob sends the dead-letter record of a message that cannot start a
// run and acknowledges the message. When the dead letter refuses the
// record, it returns the error, so the bus delivers the message again.
func (a *Agent) rejectJob(queue string, e Event, req *RunRequest, cause error) error {
	a.log.Warn("bus intake: a run request cannot start", "id", e.ID, "queue", queue, "error", cause)
	subject := ""
	if a.cfg.Bus.DeadLetterOn() {
		subject = DeadLetterOf(queue)
	}
	if subject == "" {
		return nil
	}
	d := IntakeDeadData{Queue: queue, Request: e.Data, Message: e.ID, Outcome: DeadRejected, Error: cause.Error(), Attempts: 1, Node: a.node}
	agent := "default"
	if req != nil {
		d.Attempts = attemptOf(req.Metadata)
		if req.Agent != "" {
			agent = req.Agent
		}
		r := *req
		r.Files = withoutBytes(r.Files)
		if raw, err := json.Marshal(r); err == nil {
			d.Request = raw
		}
	}
	source := fmt.Sprintf("fw://%s/%s", a.k.atespace, agent)
	if err := a.publishDead(subject, e.ID, "", source, d); err != nil {
		a.log.Warn("bus intake: the dead letter refused the record; the message comes back", "id", e.ID, "subject", subject, "error", err)
		return err
	}
	return nil
}

// publishDead sends a dead-letter record. The event id comes from the id
// of the job, so a bus that drops a repeated message drops a repeated
// record.
func (a *Agent) publishDead(subject, id, run, source string, d IntakeDeadData) error {
	d.Error = a.redact.text(d.Error)
	ev, err := NewEvent(run, EvIntakeDead, d)
	if err != nil {
		return err
	}
	ev.ID, ev.Source, ev.Node = id+DeadLetterSuffix, source, a.node
	ev.Data = a.redact.data(ev.Data)
	ctx, cancel := context.WithTimeout(context.Background(), egressTimeout)
	defer cancel()
	return a.bus.Publish(ctx, subject, ev)
}

// withoutBytes returns the blocks with the bytes of each file left out, so
// a record names a file and does not carry it.
func withoutBytes(blocks []Block) []Block {
	if !slices.ContainsFunc(blocks, func(b Block) bool { return len(b.Data) > 0 }) {
		return blocks
	}
	out := slices.Clone(blocks)
	for i := range out {
		out[i].Data = nil
	}
	return out
}

// attemptOf returns the attempt of a job from its metadata: 1 when the
// metadata names none or an attempt below 1.
func attemptOf(meta map[string]string) int {
	n, err := strconv.Atoi(meta[MetaIntakeAttempt])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// intakeGate admits a message of the intake queue only for a free slot of
// maxRuns. The drivers of top-level runs fill the slots, the ones that
// wait for a slot included, and so does each message that the gate
// admitted until its handler returns. A run that waits for a decision has
// no driver, so its slot is free for the next job. Each admission is also
// a reserved place in the backlog of maxRuns + maxQueuedRuns, which a
// start from another surface cannot take: the start of the job takes it
// over (claimPlace), so a job that the gate admitted is never refused for
// saturation. A run that another surface starts at the same moment can
// still take the working slot first; the run of the job then waits in
// this process for the next slot.
type intakeGate struct{ k *kernel }

// Admit implements Gate. It returns ErrClosed when the kernel drains.
func (g intakeGate) Admit(ctx context.Context, max int) (int, error) {
	k := g.k
	if max < 1 {
		max = 1
	}
	for {
		if err := k.writeError(); err != nil {
			return 0, err
		}
		k.slotMu.Lock()
		if free := cap(k.sem) - k.rootDrivers - k.admitted; free > 0 {
			n := min(free, max)
			k.admitted += n
			k.slotMu.Unlock()
			return n, nil
		}
		changed := k.slotsChanged
		k.slotMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-k.drainCh:
			return 0, ErrClosed
		case <-k.failedCh:
			return 0, k.storageError()
		}
	}
}

// Unused implements Gate.
func (g intakeGate) Unused(n int) { g.free(n) }

// free ends n admissions: the consumer got no message for them, or the
// handler of their message returned. A run that the handler started holds
// its slot as a driver by then.
func (g intakeGate) free(n int) {
	if n <= 0 {
		return
	}
	k := g.k
	k.slotMu.Lock()
	k.admitted = max(0, k.admitted-n)
	k.slotsChangedLocked()
	k.slotMu.Unlock()
}

// fromIntake reports whether the run is a job of the bus intake: a
// top-level run from the bus that the intake started, and not a branch of
// one.
func (rs *runState) fromIntake() bool {
	q := rs.queuedData()
	return rs.depth == 0 && q.Origin == OriginBus && q.Metadata[MetaIntakeQueue] != "" && q.Metadata[MetaForkOf] == ""
}

// outcomes sends what the failed runs of intake jobs owe: a retry or a
// dead-letter record, and then MetaIntakeOutcome on the run. One goroutine
// works through the runs in order. When the bus refuses, it tries again
// with a pause that doubles up to intakeRetryMax. At a shutdown it tries
// each run once more while the bus answers. A run that still owes its
// step at the end gets it at the next start, because the ledger holds the
// failure and no MetaIntakeOutcome.
type outcomes struct {
	a *Agent

	mu    sync.Mutex
	queue []string
	wake  chan struct{}
}

// startOutcomes watches for failed intake jobs and queues the failed jobs
// of the ledger that still owe their step. It runs before the kernel
// resumes runs, so it sees every failure of a resumed run.
func (a *Agent) startOutcomes() {
	if a.bus == nil {
		return
	}
	o := &outcomes{a: a, wake: make(chan struct{}, 1)}
	var owed []string
	for _, rs := range a.k.all() {
		if !rs.fromIntake() {
			continue
		}
		if r := rs.snapshot(); r.State == StateFailed && r.Metadata[MetaIntakeOutcome] == "" {
			owed = append(owed, rs.id)
		}
	}
	slices.Sort(owed)
	o.queue = owed
	a.Observe(ObserverFunc(o.observe))
	a.k.wg.Add(1)
	go o.loop()
	if len(owed) > 0 {
		o.signal()
	}
}

// observe queues a job whose run just failed. It only appends, because an
// observer runs while the run holds its append lock.
func (o *outcomes) observe(e Event) {
	if e.Type != EvRunFinished {
		return
	}
	var d RunFinishedData
	if e.Decode(&d) != nil || d.Outcome != OutcomeFailed {
		return
	}
	rs, err := o.a.k.lookup(e.Subject)
	if err != nil || !rs.fromIntake() {
		return
	}
	o.mu.Lock()
	o.queue = append(o.queue, e.Subject)
	o.mu.Unlock()
	o.signal()
}

func (o *outcomes) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outcomes) head() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return "", false
	}
	return o.queue[0], true
}

func (o *outcomes) pop() {
	o.mu.Lock()
	o.queue = o.queue[1:]
	o.mu.Unlock()
}

// loop settles the queued runs until the kernel closes.
func (o *outcomes) loop() {
	k := o.a.k
	defer k.wg.Done()
	delay := time.Second
	finish := func() {
		for {
			id, ok := o.head()
			if !ok {
				return
			}
			if err := o.a.settleJob(id); err != nil {
				o.a.log.Warn("bus intake: the bus refused during the shutdown; the next start sends what failed jobs owe", "run", id, "error", err)
				return
			}
			o.pop()
		}
	}
	for {
		id, ok := o.head()
		if !ok {
			select {
			case <-k.ctx.Done():
				return
			case <-k.drainCh:
				finish()
				return
			case <-o.wake:
			}
			continue
		}
		if err := o.a.settleJob(id); err != nil {
			o.a.log.Warn("bus intake: the bus refused what a failed job owes; trying again", "run", id, "error", err)
			t := time.NewTimer(delay)
			select {
			case <-k.ctx.Done():
				t.Stop()
				return
			case <-k.drainCh:
				t.Stop()
				finish()
				return
			case <-t.C:
			}
			delay = min(delay*2, intakeRetryMax)
			continue
		}
		delay = time.Second
		o.pop()
	}
}

// settleJob sends what the failed run of a job owes. With an attempt left,
// it puts the job on its queue again as a new run, which any process of
// the queue can take. After the last attempt, it sends the dead-letter
// record. Then it records MetaIntakeOutcome on the run. A run that is not
// failed now, or that has its outcome, owes nothing. The error is a
// refusal of the bus, and the caller tries again.
func (a *Agent) settleJob(id string) error {
	rs, err := a.k.lookup(id)
	if err != nil || !rs.fromIntake() {
		return nil
	}
	r := rs.snapshot()
	if r.State != StateFailed || r.Metadata[MetaIntakeOutcome] != "" {
		return nil
	}
	q := rs.queuedData()
	queue := q.Metadata[MetaIntakeQueue]
	attempt := attemptOf(q.Metadata)
	failure := r.Error
	if attempt <= a.cfg.Bus.IntakeRetries && exactSubject(queue) {
		req, err := a.retryOf(rs, q, attempt)
		if err == nil {
			ev, err := NewEvent("", EvRunRequest, req)
			if err != nil {
				return err
			}
			ev.ID, ev.Source, ev.Node = id+".retry", rs.source, a.node
			ctx, cancel := context.WithTimeout(context.Background(), egressTimeout)
			err = a.bus.Publish(ctx, queue, ev)
			cancel()
			if err != nil {
				return err
			}
			a.log.Info("bus intake: the run of a job failed; the job is on the queue again", "run", id, "queue", queue, "attempt", attempt+1, "error", failure)
			a.markJob(rs, IntakeRetried)
			return nil
		}
		failure = fmt.Sprintf("%s; no retry: %v", failure, err)
	}
	subject := ""
	if a.cfg.Bus.DeadLetterOn() {
		subject = DeadLetterOf(queue)
	}
	if subject == "" {
		a.log.Warn("bus intake: the run of a job failed after its last attempt", "run", id, "queue", queue, "attempts", attempt, "error", failure)
		a.markJob(rs, IntakeDropped)
		return nil
	}
	raw, err := json.Marshal(requestOf(q))
	if err != nil {
		return err
	}
	d := IntakeDeadData{Queue: queue, Request: raw, Run: id, Outcome: DeadFailed, Error: failure, Attempts: attempt, Node: a.node}
	if err := a.publishDead(subject, id, id, rs.source, d); err != nil {
		return err
	}
	a.log.Warn("bus intake: the run of a job failed after its last attempt; the dead letter has its record", "run", id, "subject", subject, "attempts", attempt, "error", failure)
	a.markJob(rs, IntakeDeadLettered)
	return nil
}

// markJob records MetaIntakeOutcome on the run. A failed append is logged:
// the next start then sends the step of the run again.
func (a *Agent) markJob(rs *runState, outcome string) {
	if _, err := a.emit(nil, rs, EvRunAnnotated, RunAnnotatedData{Metadata: map[string]string{MetaIntakeOutcome: outcome}}); err != nil {
		a.log.Error("bus intake: record the outcome of a job", "run", rs.id, "outcome", outcome, "error", err)
	}
}

// requestOf returns the request of a run as fw.run.queued recorded it,
// without the metadata that the kernel set for itself: the idempotency key
// goes back to its field, and the queue goes. The attempt and the failed
// run stay, so a record shows the history of the job. The files are
// references.
func requestOf(q RunQueuedData) RunRequest {
	meta := maps.Clone(q.Metadata)
	key := meta[metaIdempotencyKey]
	for _, k := range kernelMetadata {
		delete(meta, k)
	}
	delete(meta, MetaIntakeQueue)
	delete(meta, MetaIntakeOutcome)
	if len(meta) == 0 {
		meta = nil
	}
	return RunRequest{
		Agent:          q.Agent,
		Input:          q.Input,
		Instructions:   q.Instructions,
		Profile:        q.Profile,
		Budget:         q.Budget,
		Metadata:       meta,
		Principal:      q.Principal,
		PrincipalKind:  q.PrincipalKind,
		IdempotencyKey: key,
		Files:          q.Files,
	}
}

// retryOf returns the request of the next attempt of a failed job: the
// request of the run, with the attempt, the failed run, an idempotency key
// of its own, and the bytes of its files read again from the workspace.
func (a *Agent) retryOf(rs *runState, q RunQueuedData, attempt int) (RunRequest, error) {
	req := requestOf(q)
	if req.Metadata == nil {
		req.Metadata = map[string]string{}
	}
	req.Metadata[MetaIntakeAttempt] = strconv.Itoa(attempt + 1)
	req.Metadata[MetaIntakeRetryOf] = rs.id
	req.IdempotencyKey = rs.id + ".retry"
	if len(req.Files) == 0 {
		return req, nil
	}
	files := make([]Block, 0, len(req.Files))
	for _, b := range req.Files {
		if b.Type != BlockFile || b.File == nil {
			files = append(files, b)
			continue
		}
		if a.files == nil {
			return RunRequest{}, fmt.Errorf("fw: bus intake: the file %q of the run cannot be read: this agent has no workspace", b.File.Name)
		}
		data, err := fs.ReadFile(a.files, b.File.Path)
		if err != nil {
			return RunRequest{}, fmt.Errorf("fw: bus intake: the file %q of the run cannot be read: %w", b.File.Name, err)
		}
		if sum := sha256.Sum256(data); b.File.SHA256 != "" && hex.EncodeToString(sum[:]) != b.File.SHA256 {
			return RunRequest{}, fmt.Errorf("fw: bus intake: the file %q changed after the run started", b.File.Name)
		}
		files = append(files, Attachment(b.File.Name, data))
	}
	req.Files = files
	return req, nil
}

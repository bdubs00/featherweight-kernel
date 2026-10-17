package fw

// This file is the "file" ledger driver (docs/ARCHITECTURE.md section 4.3).
// Each run has a directory <root>/<run>/ with ledger.jsonl, one JSON event
// per line. The driver only appends. It calls fsync after every approval
// event and every terminal event. The file <root>/index.jsonl records each
// run id once, in the order the runs were created. The root is
// Config.Ledger.Path when set, else <Config.Home>/runs.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

func init() {
	RegisterLedger("file", func(_ context.Context, cfg *Config) (Ledger, error) {
		dir := cfg.Ledger.Path
		if dir == "" {
			home := cfg.Home
			if home == "" {
				home = DefaultHome()
			}
			dir = filepath.Join(home, "runs")
		}
		return OpenFileLedger(dir)
	})
}

const (
	ledgerFileName = "ledger.jsonl"
	indexFileName  = "index.jsonl"
)

// FileLedger is the default durable ledger: JSONL files on a local disk.
type FileLedger struct {
	dir string

	mu      sync.Mutex
	runs    map[string]*fileRun
	indexed map[string]bool

	syncs atomic.Int64 // number of fsync calls on run files, for tests
}

type fileRun struct {
	mu       sync.Mutex
	loaded   bool   // seq is known
	seq      uint64 // last sequence number in the file
	done     bool   // the run finished, and no later event opened it again
	f        *os.File
	watchers []*ledgerWatcher
}

// keepsDone reports whether an event leaves a finished run finished. An
// annotation adds metadata and opens nothing again; every other event
// after the end, such as a follow-up message, works on the run again.
func keepsDone(done bool, typ string) bool {
	return IsTerminal(typ) || (done && typ == EvRunAnnotated)
}

type indexEntry struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

// OpenFileLedger opens or creates a file ledger rooted at dir.
func OpenFileLedger(dir string) (*FileLedger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fw: file ledger: create %s: %w", dir, err)
	}
	l := &FileLedger{dir: dir, runs: map[string]*fileRun{}, indexed: map[string]bool{}}
	ids, err := l.readIndex()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		l.indexed[id] = true
	}
	return l, nil
}

// Dir returns the root directory.
func (l *FileLedger) Dir() string { return l.dir }

func (l *FileLedger) run(id string) *fileRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.runs[id]
	if r == nil {
		r = &fileRun{}
		l.runs[id] = r
	}
	return r
}

func (l *FileLedger) ledgerPath(id string) string {
	return filepath.Join(l.dir, id, ledgerFileName)
}

// Append implements Ledger.
func (l *FileLedger) Append(_ context.Context, e Event) (uint64, error) {
	if err := checkRunID(e.Subject); err != nil {
		return 0, err
	}
	r := l.run(e.Subject)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := l.loadLocked(e.Subject, r); err != nil {
		return 0, err
	}
	if err := l.index(e.Subject, e.Time); err != nil {
		return 0, err
	}
	if r.f == nil {
		if err := os.MkdirAll(filepath.Join(l.dir, e.Subject), 0o700); err != nil {
			return 0, fmt.Errorf("fw: file ledger: create run directory: %w", err)
		}
		f, err := os.OpenFile(l.ledgerPath(e.Subject), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return 0, fmt.Errorf("fw: file ledger: open %s: %w", e.Subject, err)
		}
		r.f = f
	}
	e.Seq = r.seq + 1
	if e.SpecVersion == "" {
		e.SpecVersion = SpecVersion
	}
	if e.Source == "" {
		e.Source = "fw://local/" + e.Subject
	}
	line, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("fw: file ledger: marshal %s: %w", e.Type, err)
	}
	line = append(line, '\n')
	if _, err := r.f.Write(line); err != nil {
		// A partial line is possible. The next open cuts it off.
		_ = r.f.Close()
		r.f, r.loaded = nil, false
		return 0, fmt.Errorf("fw: file ledger: append to %s: %w", e.Subject, err)
	}
	r.seq = e.Seq
	if e.Type == EvApprovalRequested || e.Type == EvApprovalDecided || IsTerminal(e.Type) {
		if err := r.f.Sync(); err != nil {
			return 0, fmt.Errorf("fw: file ledger: fsync %s: %w", e.Subject, err)
		}
		l.syncs.Add(1)
	}
	for _, w := range r.watchers {
		w.push(e)
	}
	r.done = keepsDone(r.done, e.Type)
	if r.done {
		// A finished run seldom grows again, so release its file. A
		// follow-up opens it again, and an annotation closes it again.
		_ = r.f.Close()
		r.f = nil
	}
	return e.Seq, nil
}

// loadLocked finds the last sequence number of a run. It cuts off a partial
// last line that a crash left, so the next append starts on a new line. The
// caller holds r.mu.
func (l *FileLedger) loadLocked(id string, r *fileRun) error {
	if r.loaded {
		return nil
	}
	p := l.ledgerPath(id)
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		r.seq, r.loaded = 0, true
		return nil
	}
	if err != nil {
		return fmt.Errorf("fw: file ledger: open %s: %w", id, err)
	}
	defer f.Close()
	var seq uint64
	var good int64
	done := false
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("fw: file ledger: read %s: %w", id, err)
		}
		var head struct {
			Seq  uint64 `json:"seq"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &head); err != nil {
			return fmt.Errorf("fw: file ledger: %s line %d is not an event: %w", id, seq+1, err)
		}
		seq = head.Seq
		done = keepsDone(done, head.Type)
		good += int64(len(line))
	}
	if st, err := f.Stat(); err == nil && st.Size() > good {
		if err := os.Truncate(p, good); err != nil {
			return fmt.Errorf("fw: file ledger: cut partial line of %s: %w", id, err)
		}
	}
	r.seq, r.loaded, r.done = seq, true, done
	return nil
}

// index records a new run id in index.jsonl.
func (l *FileLedger) index(id string, t time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.indexed[id] {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(l.dir, indexFileName), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fw: file ledger: open index: %w", err)
	}
	line, _ := json.Marshal(indexEntry{ID: id, Time: t.UTC()})
	line = append(line, '\n')
	_, err = f.Write(line)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("fw: file ledger: write index: %w", err)
	}
	l.indexed[id] = true
	return nil
}

// readIndex returns the run ids of index.jsonl, each once, in order. A
// partial last line is ignored.
func (l *FileLedger) readIndex() ([]string, error) {
	f, err := os.Open(filepath.Join(l.dir, indexFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fw: file ledger: open index: %w", err)
	}
	defer f.Close()
	seen := map[string]bool{}
	var ids []string
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("fw: file ledger: read index: %w", err)
		}
		var ent indexEntry
		if json.Unmarshal(line, &ent) != nil || checkRunID(ent.ID) != nil || seen[ent.ID] {
			continue
		}
		seen[ent.ID] = true
		ids = append(ids, ent.ID)
	}
	return ids, nil
}

// Read implements Ledger. A last line without a newline is a write in
// progress or a crash, and Read skips it.
func (l *FileLedger) Read(ctx context.Context, runID string, from uint64) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if err := checkRunID(runID); err != nil {
			yield(Event{}, err)
			return
		}
		f, err := os.Open(l.ledgerPath(runID))
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			yield(Event{}, fmt.Errorf("fw: file ledger: open %s: %w", runID, err))
			return
		}
		defer f.Close()
		br := bufio.NewReader(f)
		for {
			if err := ctx.Err(); err != nil {
				yield(Event{}, err)
				return
			}
			line, err := br.ReadBytes('\n')
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(Event{}, fmt.Errorf("fw: file ledger: read %s: %w", runID, err))
				return
			}
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				yield(Event{}, fmt.Errorf("fw: file ledger: %s holds a line that is not an event: %w", runID, err))
				return
			}
			if e.Seq < from {
				continue
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}

// Watch implements Ledger. It replays the file from a sequence number and
// then follows the appends of this process until ctx ends.
func (l *FileLedger) Watch(ctx context.Context, runID string, from uint64) (<-chan Event, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	r := l.run(runID)
	w := newLedgerWatcher()
	r.mu.Lock()
	if err := l.loadLocked(runID, r); err != nil {
		r.mu.Unlock()
		return nil, err
	}
	// Every event up to upto is in the file. Every later event reaches w.
	upto := r.seq
	r.watchers = append(r.watchers, w)
	r.mu.Unlock()

	go func() {
		defer close(w.ch)
		defer func() {
			r.mu.Lock()
			for i, x := range r.watchers {
				if x == w {
					r.watchers = append(r.watchers[:i], r.watchers[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
		}()
		var last uint64
		for e, err := range l.Read(ctx, runID, from) {
			if err != nil || e.Seq > upto {
				break
			}
			select {
			case w.ch <- e:
				last = e.Seq
			case <-ctx.Done():
				return
			}
		}
		w.follow(ctx, from, last)
	}()
	return w.ch, nil
}

// Runs implements Ledger. It lists the ids of index.jsonl in order.
func (l *FileLedger) Runs(_ context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		ids, err := l.readIndex()
		if err != nil {
			yield("", err)
			return
		}
		for _, id := range ids {
			if !yield(id, nil) {
				return
			}
		}
	}
}

// Flush calls fsync on every open run file and closes it. The next append
// opens the file again. The kernel calls Flush at shutdown.
func (l *FileLedger) Flush() error {
	l.mu.Lock()
	runs := make([]*fileRun, 0, len(l.runs))
	for _, r := range l.runs {
		runs = append(runs, r)
	}
	l.mu.Unlock()
	var errs []error
	for _, r := range runs {
		r.mu.Lock()
		if r.f != nil {
			if err := r.f.Sync(); err != nil {
				errs = append(errs, err)
			}
			if err := r.f.Close(); err != nil {
				errs = append(errs, err)
			}
			r.f = nil
		}
		r.mu.Unlock()
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("fw: file ledger: flush: %w", err)
	}
	return nil
}

// Close flushes the ledger. The ledger stays usable after Close.
func (l *FileLedger) Close() error { return l.Flush() }

// ledgerWatcher hands events from Append to one Watch channel without
// blocking Append.
type ledgerWatcher struct {
	mu      sync.Mutex
	pending []Event
	notify  chan struct{}
	ch      chan Event
}

func newLedgerWatcher() *ledgerWatcher {
	return &ledgerWatcher{notify: make(chan struct{}, 1), ch: make(chan Event, 64)}
}

func (w *ledgerWatcher) push(e Event) {
	w.mu.Lock()
	w.pending = append(w.pending, e)
	w.mu.Unlock()
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// follow sends pushed events after last until ctx ends.
func (w *ledgerWatcher) follow(ctx context.Context, from, last uint64) {
	for {
		w.mu.Lock()
		batch := w.pending
		w.pending = nil
		w.mu.Unlock()
		for _, e := range batch {
			if e.Seq <= last || e.Seq < from {
				continue
			}
			select {
			case w.ch <- e:
				last = e.Seq
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-w.notify:
		case <-ctx.Done():
			return
		}
	}
}

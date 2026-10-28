package fw

// This file is the "file" ledger driver (docs/ARCHITECTURE.md section 4.3).
// Each run has a directory <root>/<run>/ with ledger.jsonl, one JSON event
// per line. The driver only appends. It calls fsync after every approval
// event, every terminal event, every record of a child in another process,
// every queued run, external message, budget change, annotation, and request to
// run a tool that is not idempotent. An event whose tool intent has no
// readable classification counts as not idempotent.
// Such a success also stands on the entries behind the file: the first
// open of a run owes an fsync of
// its directory and of the root, and the first new entry of the index
// owes an fsync of the root. A failed fsync keeps the debt until a retry
// pays it, and a debt survives a path that already exists. The file
// <root>/index.jsonl records each run id once, in the order the runs were
// created. The root is Config.Ledger.Path when set, else
// <Config.Home>/runs.

import (
	"bufio"
	"bytes"
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

	// lifetime fences every mutating operation against Close. The directory
	// descriptor holds the OS writer lock until Close, never merely Flush.
	lifetime  sync.RWMutex
	owner     *os.File
	readOnly  bool
	closed    bool
	watchCtx  context.Context
	stopWatch context.CancelFunc

	// egressMu serializes the egress cursors. egressDirs holds the cursor
	// directories whose entries this ledger made durable, so each costs its
	// fsyncs once.
	egressMu   sync.Mutex
	egressDirs map[string]bool

	mu      sync.Mutex
	runs    map[string]*fileRun
	indexed map[string]bool

	reads  atomic.Int64 // number of event lines decoded by Read, for tests
	esyncs atomic.Int64 // number of fsync calls for egress cursors, for tests
	syncs  atomic.Int64 // number of fsync calls on run files, for tests
	dsyncs atomic.Int64 // number of fsync calls on run directories, for tests
	rsyncs atomic.Int64 // number of fsync calls on the ledger root, for tests

	// failFileSyncs makes that many fsync calls on files of this ledger
	// fail first, and failDirSyncs does the same for directories. Each is
	// a count of failures left, and each belongs to this ledger alone, so
	// a test can walk a failure and the retry that pays the debt.
	failFileSyncs atomic.Int64
	failDirSyncs  atomic.Int64
	beforeDirSync func(string) error // optional per-instance fault injection for setup tests

	// indexDirSync holds while the entry of index.jsonl in the root has
	// no fsync behind it. The first new entry of the index pays it.
	indexDirSync bool
}

type fileRun struct {
	mu     sync.Mutex
	loaded bool   // seq is known
	seq    uint64 // last sequence number in the file
	done   bool   // the run finished, and no later event opened it again
	// opened says the ledger file was opened once in this ledger. The
	// first open owes the fsyncs of the entries behind the file, and no
	// later open owes them again. A successful fsync pays a debt once,
	// and a failed one keeps it.
	opened   bool
	rootSync bool // the entry of the run directory owes an fsync of the root
	dirSync  bool // the entry of the ledger file owes an fsync of the directory of the run
	// cursorSynced says an egress cursor of this run paid the fsyncs of
	// the entries behind the ledger file once in this ledger. markSeq and
	// markOff are the sequence number and the byte offset of the line of
	// the last egress cursor, so a Read after it starts there.
	cursorSynced bool
	markSeq      uint64
	markOff      int64
	f            *os.File
	dirty        bool // writes still need a file sync, even if f was closed
	watchers     []*ledgerWatcher
}

// keepsDone reports whether an event leaves a finished run finished. An
// annotation and the report of a child add facts and open nothing again;
// every other event after the end, such as a follow-up message, works on
// the run again (LeavesFinished).
func keepsDone(done bool, typ string) bool {
	return IsTerminal(typ) || (done && LeavesFinished(typ))
}

// needsIntentSync reports whether an event must reach the disk before
// Append reports success. Accepted control events need it, and so
// does the intent to run a tool that is not idempotent: the kernel runs
// the body of a tool only after Append returned, and a restart that
// reads the ledger must not lose the record that the tool was asked
// for. An fw.tool.requested event without a readable classification,
// as from an older version or a direct caller, counts as not
// idempotent, so no event can bypass the durable write. A record of a
// child in another process needs it too: the kernel records a child
// before the spawner creates its process, and a restart must know every
// process that the spawner created. Queues, budget changes, annotations,
// reactions, and external follow-ups are acknowledged to their caller, so
// they need the same boundary regardless of the surface that accepted
// them. Internal kernel notes have no external acknowledgment and stay
// batched. An absent or unreadable message origin counts as external,
// never as a kernel note.
func needsIntentSync(typ string, data json.RawMessage) bool {
	if typ == EvApprovalRequested || typ == EvApprovalDecided || typ == EvRunChild ||
		typ == EvRunQueued || typ == EvBudgetChanged || typ == EvRunAnnotated || typ == EvRunReacted || IsTerminal(typ) {
		return true
	}
	if typ == EvMessageReceived {
		var m struct {
			Origin string `json:"origin"`
			Note   string `json:"note"`
		}
		if json.Unmarshal(data, &m) != nil || m.Origin != OriginKernel {
			return true
		}
		switch m.Note {
		case NoteTime, NoteLoop, NoteBudget, NoteReport, NoteOutcome, NoteDone:
			return false
		default:
			return true
		}
	}
	if typ != EvToolRequested {
		return false
	}
	var d ToolRequestedData
	if err := json.Unmarshal(data, &d); err != nil {
		return true
	}
	return !d.Idempotent
}

type indexEntry struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
}

// OpenFileLedger opens or creates a file ledger rooted at dir and takes
// exclusive, nonblocking process-safe writer ownership until Close. Path
// aliases of the same directory share ownership. This is a local filesystem
// fence, not a distributed ledger protocol. Unsupported platforms fail closed.
// It fsyncs
// the directory chain even when it exists: a previous setup may have
// created it and failed before making its entries durable.
func OpenFileLedger(dir string) (*FileLedger, error) {
	return openFileLedger(dir, nil)
}

func openFileLedger(dir string, beforeDirSync func(string) error) (*FileLedger, error) {
	l := &FileLedger{dir: dir, runs: map[string]*fileRun{}, indexed: map[string]bool{}, indexDirSync: true, beforeDirSync: beforeDirSync}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fw: file ledger: create %s: %w", dir, err)
	}
	canonical, err := filepath.Abs(dir)
	if err == nil {
		canonical, err = filepath.EvalSymlinks(canonical)
	}
	if err != nil {
		return nil, fmt.Errorf("fw: file ledger: resolve root: %w", err)
	}
	l.dir = canonical
	dir = canonical
	l.owner, err = lockLedgerDirectory(canonical)
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			_ = l.owner.Close()
		}
	}()
	if err := l.syncDirectoryChain(dir); err != nil {
		return nil, fmt.Errorf("fw: file ledger: fsync the directory chain for %s: %w", dir, err)
	}
	// An earlier index append may have failed its fsync without losing its
	// bytes from the page cache. Establish durability before trusting those
	// ids in indexed, which otherwise skips the index write on later appends.
	f, err := os.OpenFile(filepath.Join(dir, indexFileName), os.O_RDWR, 0)
	if err == nil {
		err = trimPartialLine(f)
		if err == nil {
			err = l.syncFile(f)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("fw: file ledger: recover index: %w", err)
	}
	ids, err := l.readIndex()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		l.indexed[id] = true
	}
	l.watchCtx, l.stopWatch = context.WithCancel(context.Background())
	ready = true
	return l, nil
}

// OpenFileLedgerReadOnly opens an existing root without taking writer
// ownership, repairing tails, creating paths, or syncing files. Read and Runs
// see complete records, not an atomic cross-run snapshot. Repeated reads see
// later appends from the writer. Watch and egress cursors need a writer handle;
// readers that follow another process must poll Read. Close is final.
func OpenFileLedgerReadOnly(dir string) (*FileLedger, error) {
	path, err := filepath.Abs(dir)
	if err == nil {
		path, err = filepath.EvalSymlinks(path)
	}
	if err != nil {
		return nil, fmt.Errorf("fw: file ledger: open read-only root: %w", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("fw: file ledger: open read-only root: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("fw: file ledger: read-only root is not a directory: %s", path)
	}
	return &FileLedger{dir: path, readOnly: true, runs: map[string]*fileRun{}}, nil
}

// writable is called with lifetime held.
func (l *FileLedger) writable() error {
	if l.closed {
		return ErrLedgerClosed
	}
	if l.readOnly {
		return ErrLedgerReadOnly
	}
	return nil
}

// syncFile fsyncs f. The failFileSyncs field makes that many fsync calls
// on files fail first, so a test can walk a failure and the retry.
func (l *FileLedger) syncFile(f *os.File) error {
	if l.failFileSyncs.Add(-1) >= 0 {
		return errors.New("simulated fsync failure")
	}
	return f.Sync()
}

// syncDir fsyncs a directory, so its entries survive a crash. The
// failDirSyncs field makes that many of these calls fail first.
func (l *FileLedger) syncDir(path string) error {
	if l.beforeDirSync != nil {
		if err := l.beforeDirSync(path); err != nil {
			return err
		}
	}
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	if l.failDirSyncs.Add(-1) >= 0 {
		_ = d.Close()
		return errors.New("simulated fsync failure")
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// syncDirectoryChain makes each directory reachable through a synced parent.
// Do this once per open, bottom-up through the filesystem root, including on
// retries. Path existence cannot distinguish a durable entry from one left
// by a failed setup. Resolve symlinks so the parents are the actual parents
// of the created directories.
func (l *FileLedger) syncDirectoryChain(dir string) error {
	path, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	for {
		if err := l.syncDir(path); err != nil {
			return err
		}
		l.rsyncs.Add(1)
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
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
	l.lifetime.RLock()
	defer l.lifetime.RUnlock()
	if err := l.writable(); err != nil {
		return 0, err
	}
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
		if !r.opened {
			// The first open of the ledger file of a run owes the fsyncs
			// of the entries behind it, even when the directories already
			// exist: an older process may have left them without one. A
			// later open of an established run owes nothing.
			r.rootSync, r.dirSync, r.opened = true, true, true
		}
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
	r.dirty = true
	// A failed fsync leaves the line in the file with its sequence
	// number spent, but Append reports the failure and notifies no
	// watcher: success comes only after an event that needs durability
	// has reached the disk. The fsync of the file alone keeps no entry,
	// so the success also stands on the entries behind it: the ledger
	// file in the directory of the run, then the directory in the root.
	// A failed fsync there keeps the debt, and the next append that
	// reports success pays it, however many appends later that is.
	if needsIntentSync(e.Type, e.Data) {
		if err := l.syncRunLocked(e.Subject, r); err != nil {
			return 0, err
		}
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
	f, err := os.OpenFile(filepath.Join(l.dir, indexFileName), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("fw: file ledger: open index: %w", err)
	}
	if err := trimPartialLine(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("fw: file ledger: recover index: %w", err)
	}
	line, _ := json.Marshal(indexEntry{ID: id, Time: t.UTC()})
	line = append(line, '\n')
	_, err = f.Write(line)
	if err == nil {
		err = l.syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("fw: file ledger: write index: %w", err)
	}
	// The entry of the index file in the root owes an fsync, and a failed
	// one keeps the debt: the next new entry of the index pays it.
	if l.indexDirSync {
		if err := l.syncDir(l.dir); err != nil {
			return fmt.Errorf("fw: file ledger: fsync the ledger root: %w", err)
		}
		l.indexDirSync = false
		l.rsyncs.Add(1)
	}
	l.indexed[id] = true
	return nil
}

// trimPartialLine removes only an incomplete trailing record. In particular,
// an index write retried after a partial write must start on a fresh line;
// otherwise the next run's id can disappear inside a malformed record.
func trimPartialLine(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	end := st.Size()
	var buf [4096]byte
	for end > 0 {
		start := max(end-int64(len(buf)), 0)
		part := buf[:end-start]
		if _, err := f.ReadAt(part, start); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(part, '\n'); i >= 0 {
			good := start + int64(i) + 1
			if good == st.Size() {
				return nil
			}
			return f.Truncate(good)
		}
		end = start
	}
	return f.Truncate(0)
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
		l.lifetime.RLock()
		closed := l.closed
		l.lifetime.RUnlock()
		if closed {
			yield(Event{}, ErrLedgerClosed)
			return
		}
		if err := checkRunID(runID); err != nil {
			yield(Event{}, err)
			return
		}
		// A Read after the last event ends here. A Read after the last
		// egress cursor starts at the line of the cursor and not at the
		// first line, so the egress of a long run reads only its new
		// events. The lock of the appends guards the snapshot: a later
		// append is in the next Read.
		var start int64
		var startSeq uint64
		if from > 0 && !l.readOnly {
			r := l.run(runID)
			r.mu.Lock()
			// Reads never repair a tail. Only a writer operation loads and
			// repairs it; an uncached read simply scans the complete lines.
			caughtUp := r.loaded && from > r.seq
			if r.markSeq > 0 && r.markSeq < from {
				start, startSeq = r.markOff, r.markSeq
			}
			r.mu.Unlock()
			if caughtUp {
				return
			}
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
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			yield(Event{}, fmt.Errorf("fw: file ledger: read %s: %w", runID, err))
			return
		}
		br := bufio.NewReader(f)
		check := start > 0
		for {
			if err := ctx.Err(); err != nil {
				yield(Event{}, err)
				return
			}
			line, err := br.ReadBytes('\n')
			if err != nil && err != io.EOF {
				yield(Event{}, fmt.Errorf("fw: file ledger: read %s: %w", runID, err))
				return
			}
			var e Event
			uerr := io.EOF
			if err == nil {
				uerr = json.Unmarshal(line, &e)
			}
			if check {
				check = false
				if uerr != nil || e.Seq != startSeq {
					// The hint does not hold. Read from the first line.
					if _, err := f.Seek(0, io.SeekStart); err != nil {
						yield(Event{}, fmt.Errorf("fw: file ledger: read %s: %w", runID, err))
						return
					}
					br.Reset(f)
					continue
				}
			}
			if err == io.EOF {
				return
			}
			if uerr != nil {
				yield(Event{}, fmt.Errorf("fw: file ledger: %s holds a line that is not an event: %w", runID, uerr))
				return
			}
			l.reads.Add(1)
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
	l.lifetime.RLock()
	defer l.lifetime.RUnlock()
	if err := l.writable(); err != nil {
		return nil, err
	}
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

	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.watchCtx, cancel)
	go func() {
		defer cancel()
		defer stop()
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
		l.lifetime.RLock()
		closed := l.closed
		l.lifetime.RUnlock()
		if closed {
			yield("", ErrLedgerClosed)
			return
		}
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

// Flush syncs outstanding run writes and directory entries and closes run
// files. It retains writer ownership and stays reusable. A read-only Flush
// does nothing. Close, not Flush, ends ownership.
func (l *FileLedger) Flush() error {
	l.lifetime.Lock()
	defer l.lifetime.Unlock()
	if l.closed {
		return ErrLedgerClosed
	}
	return l.flushLocked()
}

func (l *FileLedger) flushLocked() error {
	l.mu.Lock()
	runs := make(map[string]*fileRun, len(l.runs))
	for id, r := range l.runs {
		runs[id] = r
	}
	l.mu.Unlock()
	var errs []error
	for id, r := range runs {
		r.mu.Lock()
		if r.dirty || r.dirSync || r.rootSync {
			if err := l.syncRunLocked(id, r); err != nil {
				errs = append(errs, err)
			}
		}
		if r.f != nil {
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

// Close flushes and permanently closes the ledger, ends watches, and releases
// writer ownership even if flushing fails. Repeated Close calls do nothing.
// An in-flight Read can finish; no new operation starts after Close.
func (l *FileLedger) Close() error {
	l.lifetime.Lock()
	defer l.lifetime.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.stopWatch != nil {
		l.stopWatch()
	}
	err := l.flushLocked()
	if l.owner != nil {
		err = errors.Join(err, l.owner.Close())
		l.owner = nil
	}
	return err
}

// syncRunLocked pays both the file and entry debts. The caller holds r.mu
// and lifetime. A failed Flush can close f, so retry through a fresh descriptor.
func (l *FileLedger) syncRunLocked(id string, r *fileRun) error {
	f := r.f
	if f == nil {
		var err error
		f, err = os.Open(l.ledgerPath(id))
		if err != nil {
			return fmt.Errorf("fw: file ledger: open %s for sync: %w", id, err)
		}
		defer f.Close()
	}
	if err := l.syncFile(f); err != nil {
		return fmt.Errorf("fw: file ledger: fsync %s: %w", id, err)
	}
	l.syncs.Add(1)
	if r.dirSync {
		if err := l.syncDir(filepath.Join(l.dir, id)); err != nil {
			return fmt.Errorf("fw: file ledger: fsync the directory of %s: %w", id, err)
		}
		r.dirSync = false
		l.dsyncs.Add(1)
	}
	if r.rootSync {
		if err := l.syncDir(l.dir); err != nil {
			return fmt.Errorf("fw: file ledger: fsync the ledger root for %s: %w", id, err)
		}
		r.rootSync = false
		l.rsyncs.Add(1)
	}
	r.dirty = false
	return nil
}

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

package fw

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// This file holds the egress cursors of the file ledger (EgressCursors).
// A cursor is a small JSON file under <dir>/.egress/<route>/<run>, with
// both names hashed, so neither the configuration of a route nor the id of
// a run becomes a path, and Runs never meets a cursor.

func (l *FileLedger) egressCursorPath(route, runID string) string {
	return filepath.Join(l.dir, ".egress", fmt.Sprintf("%x", sha256.Sum256([]byte(route))), fmt.Sprintf("%x.json", sha256.Sum256([]byte(runID))))
}

// fileEgressCursor is the content of a cursor file. ID binds the cursor to
// the event at Seq, so a ledger that a recovery cut and then extended
// under the same sequence numbers does not match. Offset is the byte
// offset of that line, a hint that a load checks before it trusts it.
type fileEgressCursor struct {
	Seq    uint64 `json:"seq"`
	ID     string `json:"id"`
	Offset *int64 `json:"offset,omitempty"`
}

// cursorEventAt reads the event at a byte offset of a ledger file. It
// reports false for a missing, negative, or too large offset, for an
// offset that is not at the start of a line, and for a line that is not
// an event.
func cursorEventAt(f *os.File, offset *int64) (Event, bool, error) {
	if offset == nil || *offset < 0 {
		return Event{}, false, nil
	}
	st, err := f.Stat()
	if err != nil {
		return Event{}, false, err
	}
	if *offset >= st.Size() {
		return Event{}, false, nil
	}
	if *offset > 0 {
		var prev [1]byte
		if _, err := f.ReadAt(prev[:], *offset-1); err != nil {
			return Event{}, false, err
		}
		if prev[0] != '\n' {
			return Event{}, false, nil
		}
	}
	if _, err := f.Seek(*offset, io.SeekStart); err != nil {
		return Event{}, false, err
	}
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if errors.Is(err, io.EOF) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return Event{}, false, nil
	}
	return ev, true, nil
}

// resetEgressCursor removes a cursor that does not match the ledger, so
// the egress sends the run again from the start. The caller holds
// egressMu.
func (l *FileLedger) resetEgressCursor(path string) (uint64, error) {
	if err := os.Remove(path); err != nil {
		return 0, err
	}
	l.esyncs.Add(1)
	return 0, l.syncDir(filepath.Dir(path))
}

// LoadEgressCursor implements EgressCursors.
func (l *FileLedger) LoadEgressCursor(ctx context.Context, route, runID string) (uint64, error) {
	l.lifetime.RLock()
	defer l.lifetime.RUnlock()
	if err := l.writable(); err != nil {
		return 0, err
	}
	if err := checkRunID(runID); err != nil {
		return 0, err
	}
	l.egressMu.Lock()
	defer l.egressMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path := l.egressCursorPath(route, runID)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var cursor fileEgressCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return l.resetEgressCursor(path)
	}
	// A save can rename the file and then fail the fsync of its directory,
	// in this ledger or in the process before. The load pays that debt
	// before it trusts the file, once per directory.
	dir := filepath.Dir(path)
	if !l.egressDirs[dir] {
		l.esyncs.Add(1)
		if err := l.syncDir(dir); err != nil {
			return 0, err
		}
		l.setEgressDir(dir, true)
	}
	r := l.run(runID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := l.loadLocked(runID, r); err != nil {
		return 0, err
	}
	if cursor.Seq > r.seq {
		return l.resetEgressCursor(path)
	}
	f, err := os.Open(l.ledgerPath(runID))
	if errors.Is(err, fs.ErrNotExist) {
		return l.resetEgressCursor(path)
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	ev, valid, err := cursorEventAt(f, cursor.Offset)
	if err != nil {
		return 0, err
	}
	if !valid || ev.Seq != cursor.Seq || ev.ID != cursor.ID {
		// A recovery can cut the ledger and append again under the same
		// sequence numbers before the egress loads the cursor. The cursor
		// goes, so no event of the new tail is skipped.
		return l.resetEgressCursor(path)
	}
	if cursor.Seq > r.markSeq {
		r.markSeq, r.markOff = cursor.Seq, *cursor.Offset
	}
	return cursor.Seq, nil
}

func (l *FileLedger) setEgressDir(dir string, durable bool) {
	if l.egressDirs == nil {
		l.egressDirs = map[string]bool{}
	}
	if durable {
		l.egressDirs[dir] = true
	} else {
		delete(l.egressDirs, dir)
	}
}

// findEgressCursor finds the line of seq in a ledger file. It starts at
// the hint, the offset of an earlier line hintSeq, when the line there
// holds that event, and at the first line otherwise. It holds one line in
// memory at a time.
func findEgressCursor(ctx context.Context, f *os.File, seq, hintSeq uint64, hintOff int64) (fileEgressCursor, error) {
	var offset int64
	if hintSeq > 0 && hintSeq <= seq {
		ev, valid, err := cursorEventAt(f, &hintOff)
		if err != nil {
			return fileEgressCursor{}, err
		}
		if valid && ev.Seq == hintSeq {
			offset = hintOff
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fileEgressCursor{}, err
	}
	br := bufio.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return fileEgressCursor{}, err
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			return fileEgressCursor{}, fmt.Errorf("fw: file ledger: find the egress cursor %d: %w", seq, err)
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fileEgressCursor{}, fmt.Errorf("fw: file ledger: find the egress cursor %d: %w", seq, err)
		}
		if ev.Seq == seq {
			return fileEgressCursor{Seq: seq, ID: ev.ID, Offset: &offset}, nil
		}
		offset += int64(len(line))
	}
}

// SaveEgressCursor implements EgressCursors. The caller saves only a
// sequence number whose events the bus confirmed or the filter skipped,
// and one egress owns each route.
func (l *FileLedger) SaveEgressCursor(ctx context.Context, route, runID string, seq uint64) error {
	l.lifetime.RLock()
	defer l.lifetime.RUnlock()
	if err := l.writable(); err != nil {
		return err
	}
	if err := checkRunID(runID); err != nil {
		return err
	}
	l.egressMu.Lock()
	defer l.egressMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	r := l.run(runID)
	r.mu.Lock()
	err := l.loadLocked(runID, r)
	last := r.seq
	needDir, needRoot := r.dirSync || !r.cursorSynced, r.rootSync || !r.cursorSynced
	hintSeq, hintOff := r.markSeq, r.markOff
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if seq > last {
		return fmt.Errorf("fw: file ledger: egress cursor %d exceeds run %s sequence %d", seq, runID, last)
	}

	// Append calls fsync only for the events that need it. The cursor must
	// not be durable before the events behind it, or a crash could lose
	// them and the run could give their sequence numbers to new events
	// behind a cursor that skips them. So the file, and once per ledger
	// the entries of the file and of its directory, reach the disk first.
	// The run lock is not held for these, so an append does not wait.
	f, err := os.Open(l.ledgerPath(runID))
	if err != nil {
		return err
	}
	cursor, err := findEgressCursor(ctx, f, seq, hintSeq, hintOff)
	if err == nil {
		l.esyncs.Add(1)
		err = l.syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if needDir {
		l.esyncs.Add(1)
		if err := l.syncDir(filepath.Join(l.dir, runID)); err != nil {
			return err
		}
	}
	if needRoot {
		l.esyncs.Add(1)
		if err := l.syncDir(l.dir); err != nil {
			return err
		}
	}
	r.mu.Lock()
	if needDir {
		r.dirSync = false
	}
	if needRoot {
		r.rootSync = false
	}
	r.cursorSynced = true
	if cursor.Seq > r.markSeq {
		r.markSeq, r.markOff = cursor.Seq, *cursor.Offset
	}
	r.mu.Unlock()

	path := l.egressCursorPath(route, runID)
	dir := filepath.Dir(path)
	if !l.egressDirs[dir] {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		// Each directory of the chain must be durable before a cursor in
		// it can be.
		l.esyncs.Add(1)
		if err := l.syncDirectoryChain(dir); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	data, _ := json.Marshal(cursor)
	_, err = tmp.Write(data)
	if err == nil {
		l.esyncs.Add(1)
		err = l.syncFile(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	l.esyncs.Add(1)
	if err := l.syncDir(dir); err != nil {
		// The new name can be visible without being durable. The next
		// load pays the fsync before it trusts the file.
		l.setEgressDir(dir, false)
		return err
	}
	l.setEgressDir(dir, true)
	return nil
}

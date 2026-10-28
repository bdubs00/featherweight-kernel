package memory

// This file sends each change of a note to the front, when a front keeps
// the notes (docs/KNOWHOW.md section 4.2). Every change through the Store
// goes through it: the memory tool, the native API, and the hosted
// fw-memory server. The push happens inside the lock of the scope, after
// the write to the copy. A 412 of the front puts the text of the front in
// the copy and fails with ErrFrontConflict. A front that does not answer
// gets the change undone in the copy, and the change fails with
// ErrNotKept.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

// errDirAtFront refuses a delete or a rename of a directory when a front
// keeps the notes: each note that changes has its own change at the front.
var errDirAtFront = errors.New("it is a directory; the notes are kept at the front of the fleet, so delete and rename take one note at a time")

// hashOf returns the hex SHA-256 of data.
func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pushCtx returns the context of one change at the front.
func (f *noteFiles) pushCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(f.ctx, pushWait)
}

// push sends the new text of the note name to the front. old is the text
// of the copy before the write, when existed. On a conflict, the copy
// takes the text of the front; on any other failure, the copy gets old
// back, or loses the new note.
func (f *noteFiles) push(name string, data []byte, hash string, old []byte, existed bool) error {
	ifHash := "absent"
	if existed {
		ifHash = hashOf(old)
	}
	by, run := byOf(f.ctx)
	ctx, cancel := f.pushCtx()
	defer cancel()
	err := f.store.front.put(ctx, f.scope, name, frontPut{Text: string(data), IfHash: ifHash, Sources: sourcesOf(f.links), By: by, Run: run})
	switch {
	case err == nil:
		f.updateState(func(st *scopeState) bool {
			prev := st.Front[name]
			if st.Front == nil {
				st.Front = map[string]FrontNote{}
			}
			st.Front[name] = FrontNote{Name: name, Hash: hash, By: by, At: time.Now().UTC(), Uses: prev.Uses, Sources: len(sourcesOf(f.links))}
			return true
		})
		return nil
	case errors.Is(err, ErrFrontConflict):
		f.takeFront(ctx, name, old, existed)
		return ErrFrontConflict
	}
	f.restore(name, old, existed)
	return fmt.Errorf("%w (%v)", ErrNotKept, err)
}

// pushRemove puts the note name away at the front, after the copy removed
// it. old is its text. On a conflict, the copy takes the text of the
// front; on any other failure, the copy gets the note back.
func (f *noteFiles) pushRemove(name string, old []byte) error {
	ctx, cancel := f.pushCtx()
	defer cancel()
	err := f.store.front.remove(ctx, f.scope, name, hashOf(old))
	switch {
	case err == nil:
		f.updateState(func(st *scopeState) bool {
			delete(st.Front, name)
			return true
		})
		return nil
	case errors.Is(err, ErrFrontConflict):
		f.takeFront(ctx, name, old, true)
		return ErrFrontConflict
	}
	f.restore(name, old, true)
	return fmt.Errorf("%w (%v)", ErrNotKept, err)
}

// pushRename sends the note under its new name to the front and then puts
// the old name away, after the copy renamed it. data is its text. When the
// front does not take the new name, the copy renames the note back. When
// the front takes the new name and not the removal of the old one, both
// names hold the note, at the front and in the copy.
func (f *noteFiles) pushRename(oldname, newname string, data []byte) error {
	hash := hashOf(data)
	links := f.extraOf(oldname, hash).Links
	by, run := byOf(f.ctx)
	ctx, cancel := f.pushCtx()
	defer cancel()
	err := f.store.front.put(ctx, f.scope, newname, frontPut{Text: string(data), IfHash: "absent", Sources: sourcesOf(links), By: by, Run: run})
	if err != nil {
		if rerr := f.Files.Rename(newname, oldname); rerr == nil {
			_ = syncParent(f.dir, newname)
			_ = syncParent(f.dir, oldname)
		}
		if errors.Is(err, ErrFrontConflict) {
			f.takeFront(ctx, newname, nil, false)
			return ErrFrontConflict
		}
		return fmt.Errorf("%w (%v)", ErrNotKept, err)
	}
	f.updateState(func(st *scopeState) bool {
		if st.Front == nil {
			st.Front = map[string]FrontNote{}
		}
		st.Front[newname] = FrontNote{Name: newname, Hash: hash, By: by, At: time.Now().UTC(), Sources: len(links)}
		return true
	})
	err = f.store.front.remove(ctx, f.scope, oldname, hash)
	switch {
	case err == nil:
		f.updateState(func(st *scopeState) bool {
			delete(st.Front, oldname)
			return true
		})
		return nil
	case errors.Is(err, ErrFrontConflict):
		f.takeFront(ctx, oldname, data, true)
	default:
		f.restore(oldname, data, true)
	}
	return fmt.Errorf("%s now holds the note, and %s stays, because the front did not put the old name away: %w", Prefix+"/"+newname, Prefix+"/"+oldname, err)
}

// takeFront puts the text of the front for the note name in the copy, after
// the front refused a change with 412. A note that the front does not have
// leaves the copy. When the front does not answer, the copy gets old back,
// or loses the note when it did not exist.
func (f *noteFiles) takeFront(ctx context.Context, name string, old []byte, existed bool) {
	rec, found, err := f.store.front.get(ctx, f.scope, name)
	switch {
	case err != nil:
		f.restore(name, old, existed)
	case !found || len(rec.Text) > MaxNoteBytes:
		f.restore(name, nil, false)
		f.updateState(func(st *scopeState) bool {
			delete(st.Front, name)
			delete(st.Notes, name)
			return true
		})
	default:
		if writeAtomic(f.dir, name, []byte(rec.Text)) != nil {
			return
		}
		hash := hashOf([]byte(rec.Text))
		f.updateState(func(st *scopeState) bool {
			if st.Front == nil {
				st.Front = map[string]FrontNote{}
			}
			prev := st.Front[name]
			st.Front[name] = FrontNote{Name: name, Hash: hash, State: rec.State, By: prev.By, At: prev.At, Uses: prev.Uses, Sources: len(rec.Sources)}
			setExtra(st, name, noteExtra{Hash: hash, Links: linksOf(rec.Sources)})
			return true
		})
	}
	f.store.dropFromIndex(f.ctx, f.scope, name)
}

// restore puts the text old back in the copy at name, or removes the note
// at name when it did not exist. A failure leaves the copy to the next
// pull, which takes the text of the front.
func (f *noteFiles) restore(name string, old []byte, existed bool) {
	if existed {
		_ = writeAtomic(f.dir, name, old)
		return
	}
	if err := f.Files.Remove(name); err == nil {
		_ = syncParent(f.dir, name)
	}
}

// updateState changes the state of the scope with fn and writes it when fn
// reports a change. The caller holds the lock of the scope. A state that
// cannot be written leaves the notes as they are.
func (f *noteFiles) updateState(fn func(st *scopeState) bool) {
	st := f.readState()
	if fn(&st) {
		_ = f.writeState(st)
	}
}

// setExtra sets or drops what a note keeps beside its text.
func setExtra(st *scopeState, name string, e noteExtra) {
	if len(e.Links) == 0 && len(e.Metadata) == 0 {
		delete(st.Notes, name)
		return
	}
	if st.Notes == nil {
		st.Notes = map[string]noteExtra{}
	}
	st.Notes[name] = e
}

// extraOf returns what the note name keeps beside the text of hash.
func (f *noteFiles) extraOf(name, hash string) noteExtra {
	if e, ok := f.readState().Notes[name]; ok && e.Hash == hash {
		return e
	}
	return noteExtra{}
}

// keepExtra keeps the sources and the fields of a write through Remember
// for the text of hash, and drops those of an earlier text.
func (f *noteFiles) keepExtra(name, hash string) {
	f.updateState(func(st *scopeState) bool {
		_, had := st.Notes[name]
		if !had && len(f.links) == 0 && len(f.meta) == 0 {
			return false
		}
		setExtra(st, name, noteExtra{Hash: hash, Links: f.links, Metadata: f.meta})
		return true
	})
}

// dropExtra drops what the note name, or every note under the directory
// name, keeps beside its text.
func (f *noteFiles) dropExtra(name string) {
	f.updateState(func(st *scopeState) bool {
		changed := false
		for k := range st.Notes {
			if k == name || strings.HasPrefix(k, name+"/") {
				delete(st.Notes, k)
				changed = true
			}
		}
		return changed
	})
}

// moveExtra moves what a note, or every note under a directory, keeps
// beside its text to its new name.
func (f *noteFiles) moveExtra(oldname, newname string) {
	f.updateState(func(st *scopeState) bool {
		changed := false
		for k, e := range maps.Clone(st.Notes) {
			switch {
			case k == oldname:
				delete(st.Notes, k)
				st.Notes[newname] = e
				changed = true
			case strings.HasPrefix(k, oldname+"/"):
				delete(st.Notes, k)
				st.Notes[newname+strings.TrimPrefix(k, oldname)] = e
				changed = true
			}
		}
		return changed
	})
}

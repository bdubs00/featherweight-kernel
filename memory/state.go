package memory

// This file holds the state of the store in a scope: one hidden file next
// to the notes. The walks, the index, the listings, and the tools skip a
// hidden file, and no note name starts with '.', so neither a model nor a
// caller of the native API reads or writes it. The store reads and writes
// it only while it holds the lock of the scope.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// ConflictText is the sentence of an error that a write gets when the
// note changed in another run since the caller read it.
const ConflictText = "This note changed in another run. View it again before you change it."

// stateName is the hidden file of a scope that holds the state of the
// store.
const stateName = ".fw-state.json"

// maxEdits bounds the edits of the memory tool that a scope remembers.
// A repeated call comes after a restart, soon after the first one.
const maxEdits = 64

// scopeState is the state of the store in one scope.
type scopeState struct {
	// Edits are the last edits of the memory tool, oldest first
	// (CallEdit).
	Edits []CallEdit `json:"edits,omitempty"`
	// Notes holds the sources and the fields of a note, by name, for the
	// text of one hash (noteExtra).
	Notes map[string]noteExtra `json:"notes,omitempty"`
	// Front holds what the copy knows of the notes at the front, by name:
	// the list of the last pull, with the changes that the copy pushed
	// since. A note that the front no longer lists and that is here went
	// away at the front, so the next pull removes its copy.
	Front map[string]FrontNote `json:"front,omitempty"`
	// Hashes keeps the hash of each note with the size, the time, and the
	// inode of its file, so a pull hashes only the notes that changed.
	Hashes map[string]hashStat `json:"hashes,omitempty"`
}

// noteExtra is what a note keeps beside its text: the sources
// (MemoryRecord.Links) and the fields (MemoryRecord.Metadata) of the write
// that gave the note the text of Hash. A later write with another text
// drops them, because they describe the text that had them.
type noteExtra struct {
	Hash     string           `json:"hash"`
	Links    []fw.MemoryLink  `json:"links,omitempty"`
	Metadata []fw.MemoryField `json:"metadata,omitempty"`
}

// hashStat is the hash of a note file with what changes when the file
// changes.
type hashStat struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Mod  int64  `json:"mod"` // nanoseconds since the epoch
	Ino  uint64 `json:"ino,omitempty"`
}

// readStateAt reads the state of the scope in dir without the lock, for a
// reader such as Get. The store replaces the file atomically, so a reader
// sees the old state or the new one.
func readStateAt(dir string) scopeState {
	files, err := fw.DirFiles(dir)
	if err != nil {
		return scopeState{}
	}
	return (&noteFiles{Files: files, dir: dir}).readState()
}

// CallEdit is the record of one edit of a note by a call of the memory
// tool: insert or str_replace. The tool records it before the write, so a
// repeated call after a restart can tell whether the first one wrote
// (docs/KNOWHOW.md section 4.3). It holds the result of the first call,
// which holds lines of the note, so it stays in the scope of the note.
type CallEdit struct {
	CallID string    `json:"callId"`
	Name   string    `json:"name"`   // the note, relative to the scope
	Before string    `json:"before"` // the hex SHA-256 of the note before the edit
	After  string    `json:"after"`  // the hex SHA-256 of the note after the edit
	Result string    `json:"result"` // the text that the first call returned
	At     time.Time `json:"at"`
}

// readState reads the state of the scope. A missing or unreadable file is
// an empty state: the state helps a repeat and the copy of a front, and a
// lost state never loses a note.
func (f *noteFiles) readState() scopeState {
	var st scopeState
	data, err := fs.ReadFile(f.Files, stateName)
	if err != nil || json.Unmarshal(data, &st) != nil {
		return scopeState{}
	}
	return st
}

// writeState writes the state of the scope atomically. The caller holds
// the lock of the scope.
func (f *noteFiles) writeState(st scopeState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeAtomic(f.dir, stateName, data)
}

// lockedFiles returns the notes of files for a caller that holds the lock
// of the scope (Locked), or an error.
func lockedFiles(files fw.Files) (*noteFiles, error) {
	f, ok := files.(*noteFiles)
	if !ok || !f.locked {
		return nil, errors.New("memory: the state of a scope needs the files of Locked")
	}
	return f, nil
}

// EditOf returns the edit that the call callID recorded in the scope of
// files, which must come from Locked.
func EditOf(files fw.Files, callID string) (CallEdit, bool, error) {
	f, err := lockedFiles(files)
	if err != nil {
		return CallEdit{}, false, err
	}
	for _, e := range f.readState().Edits {
		if e.CallID == callID {
			return e, true, nil
		}
	}
	return CallEdit{}, false, nil
}

// RecordEdit records an edit in the scope of files, which must come from
// Locked, in place of an earlier record of the same call. The scope keeps
// the last maxEdits edits.
func RecordEdit(files fw.Files, e CallEdit) error {
	f, err := lockedFiles(files)
	if err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	st := f.readState()
	kept := st.Edits[:0]
	for _, x := range st.Edits {
		if x.CallID != e.CallID {
			kept = append(kept, x)
		}
	}
	st.Edits = append(kept, e)
	if n := len(st.Edits) - maxEdits; n > 0 {
		st.Edits = st.Edits[n:]
	}
	if err := f.writeState(st); err != nil {
		return fmt.Errorf("memory: record the edit: %w", err)
	}
	return nil
}

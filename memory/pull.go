package memory

// This file takes the notes of the front into the copy when a run starts,
// and builds the block of the notes from them (docs/KNOWHOW.md sections
// 4.2 and 4.4).

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// NoAnswerNote is the line of the block of the notes when the front did not
// answer the pull within PullWait.
const NoAnswerNote = "The front of the fleet did not answer. This list comes from the copy in this Task and can miss recent changes."

// PullResult says what a pull did.
type PullResult struct {
	// Answered is true when the front answered with its list.
	Answered bool
	// Fetched counts the notes that the copy took from the front, Removed
	// those that it removed, and Sent those that it sent to the front
	// because the front never had them.
	Fetched, Removed, Sent int
	// Missed counts the notes that the front listed and that the copy
	// could not take.
	Missed int
}

// Pull takes the notes of the front into the copy of a scope: it asks the
// front for its list, fetches each note whose hash differs from the copy,
// and removes each copy that the front put away, holds back for a person,
// or no longer lists. A note of the copy that the front never had, such as
// a note from before the front was set, goes to the front as a new note.
// The pull takes at most PullWait. A front that does not answer leaves the
// copy as it is. Without a front, or for the shared scope, it does nothing.
func (s *Store) Pull(ctx context.Context, scope fw.MemoryScope) PullResult {
	if !s.Front(scope) {
		return PullResult{}
	}
	ctx, cancel := context.WithTimeout(ctx, PullWait)
	defer cancel()
	list, err := s.front.list(ctx, scope)
	if err != nil {
		return PullResult{}
	}
	res := PullResult{Answered: true}
	files, err := s.notes(ctx, scope)
	if err != nil {
		res.Missed = len(list)
		return res
	}
	listed := map[string]FrontNote{}
	for _, n := range list {
		if CheckName(n.Name) == nil {
			listed[n.Name] = n
		}
	}
	before, err := s.localHashes(ctx, files)
	if err != nil {
		res.Missed = len(listed)
		return res
	}
	fetched := map[string]frontRecord{}
	for _, name := range slices.Sorted(maps.Keys(listed)) {
		n := listed[name]
		if !n.kept() || before[name] == n.Hash {
			continue
		}
		rec, found, err := s.front.get(ctx, scope, name)
		switch {
		case err != nil || len(rec.Text) > MaxNoteBytes:
			res.Missed++
		case found:
			fetched[name] = rec
		default:
			n.State = StatePutAway
			listed[name] = n
		}
	}

	unlock, err := lockDir(ctx, files.dir)
	if err != nil {
		res.Missed += len(fetched)
		return res
	}
	defer unlock()
	files.locked = true
	defer func() { files.locked = false }()
	st := files.readState()
	known := st.Front
	// unchanged reports whether the note of the copy is as the pull saw it
	// before it took the lock: no write came between.
	unchanged := func(name string) bool {
		h, err := hashFile(files.Files, name)
		if prev, ok := before[name]; ok {
			return err == nil && h == prev
		}
		return errors.Is(err, fs.ErrNotExist)
	}
	for _, name := range slices.Sorted(maps.Keys(fetched)) {
		rec := fetched[name]
		if !unchanged(name) {
			res.Missed++
			continue
		}
		if _, ok := before[name]; !ok && len(before)+res.Fetched >= MaxNotes {
			res.Missed++
			continue
		}
		if writeAtomic(files.dir, name, []byte(rec.Text)) != nil {
			res.Missed++
			continue
		}
		res.Fetched++
		delete(st.Hashes, name)
		hash := hashOf([]byte(rec.Text))
		setExtra(&st, name, noteExtra{Hash: hash, Links: linksOf(rec.Sources)})
		n := listed[name]
		n.Hash = hash
		listed[name] = n
		s.dropFromIndex(ctx, scope, name)
	}
	var send []string
	for _, name := range slices.Sorted(maps.Keys(before)) {
		n, isListed := listed[name]
		_, wasKnown := known[name]
		switch {
		case isListed && n.kept():
			continue
		case !isListed && !wasKnown:
			send = append(send, name)
			continue
		}
		if !unchanged(name) || files.Files.Remove(name) != nil {
			continue
		}
		_ = syncParent(files.dir, name)
		delete(st.Notes, name)
		delete(st.Hashes, name)
		s.dropFromIndex(ctx, scope, name)
		res.Removed++
	}
	st.Front = map[string]FrontNote{}
	for name, n := range listed {
		if n.kept() {
			st.Front[name] = n
		}
	}
	for _, name := range send {
		data, err := fs.ReadFile(files.Files, name)
		if err != nil {
			continue
		}
		hash := hashOf(data)
		links := st.Notes[name]
		if links.Hash != hash {
			links = noteExtra{}
		}
		err = s.front.put(ctx, scope, name, frontPut{Text: string(data), IfHash: "absent", Sources: sourcesOf(links.Links), By: "agent"})
		switch {
		case err == nil:
			st.Front[name] = FrontNote{Name: name, Hash: hash, By: "agent", Sources: len(links.Links)}
			res.Sent++
		case errors.Is(err, ErrFrontConflict):
			// The front has a note of that name that it does not list, such
			// as a note that it put away: the copy follows the front.
			rec, found, err := s.front.get(ctx, scope, name)
			switch {
			case err != nil:
			case !found:
				if files.Files.Remove(name) == nil {
					_ = syncParent(files.dir, name)
					delete(st.Notes, name)
					delete(st.Hashes, name)
					s.dropFromIndex(ctx, scope, name)
					res.Removed++
				}
			case len(rec.Text) <= MaxNoteBytes && writeAtomic(files.dir, name, []byte(rec.Text)) == nil:
				h := hashOf([]byte(rec.Text))
				st.Front[name] = FrontNote{Name: name, Hash: h, State: rec.State, Sources: len(rec.Sources)}
				setExtra(&st, name, noteExtra{Hash: h, Links: linksOf(rec.Sources)})
				delete(st.Hashes, name)
				s.dropFromIndex(ctx, scope, name)
				res.Fetched++
			}
		}
	}
	_ = files.writeState(st)
	return res
}

// localHashes returns the hash of each note of the copy. A note whose
// size, time, and inode match the state of the scope keeps the hash that
// the state holds, so a pull reads only the notes that changed. It keeps
// the hashes in the state for the next pull.
func (s *Store) localHashes(ctx context.Context, files *noteFiles) (map[string]string, error) {
	notes, _, err := walkNotes(ctx, files.dir, 0)
	if err != nil {
		return nil, err
	}
	st := files.readState()
	out := make(map[string]string, len(notes))
	cache := make(map[string]hashStat, len(notes))
	changed := false
	for _, n := range notes {
		c, ok := st.Hashes[n.name]
		if !ok || c.Size != n.size || c.Mod != n.mod.UnixNano() || c.Ino != n.ino {
			h, err := hashFile(files.Files, n.name)
			if err != nil {
				continue
			}
			c, changed = hashStat{Hash: h, Size: n.size, Mod: n.mod.UnixNano(), Ino: n.ino}, true
		}
		out[n.name] = c.Hash
		cache[n.name] = c
	}
	if changed || len(cache) != len(st.Hashes) {
		if unlock, err := lockDir(ctx, files.dir); err == nil {
			cur := files.readState()
			cur.Hashes = cache
			_ = files.writeState(cur)
			unlock()
		}
	}
	return out, nil
}

// frontBrief is Brief with a front: it pulls the notes of the front, then
// lists the notes of the copy in three groups, each note with its label:
// notes that a person wrote ("(from a person)"), checked notes ("(checked
// <date>)"), and the other notes. Inside a group, the notes that runs used
// most come first, then the newest. Notes that are out of date or resting
// stay out of the list, and one line counts them. The first line says how
// many notes the copy keeps and how much of the bound the list fills. A
// front that did not answer adds NoAnswerNote, and the labels then come
// from the last answer of the front.
func (s *Store) frontBrief(ctx context.Context, scope fw.MemoryScope, hint string) (Brief, error) {
	pulled := s.Pull(ctx, scope)
	dir, err := s.Dir(scope)
	if err != nil {
		return Brief{}, err
	}
	notes, cut, err := walkNotes(ctx, dir, -1)
	if err != nil {
		return Brief{}, fmt.Errorf("memory: index of %s: %w", dir, err)
	}
	if len(notes) == 0 && !cut && pulled.Answered {
		return Brief{}, nil
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return Brief{}, err
	}
	st := readStateAt(dir)
	type entry struct {
		note
		hash  string
		group int
		label string
		uses  int
	}
	var list []entry
	outOfDate, resting := 0, 0
	for _, n := range notes {
		hash := ""
		if c, ok := st.Hashes[n.name]; ok && c.Size == n.size && c.Mod == n.mod.UnixNano() && c.Ino == n.ino {
			hash = c.Hash
		} else if h, err := hashFile(files, n.name); err == nil {
			hash = h
		}
		e := entry{note: n, hash: hash, group: 2}
		if f, ok := st.Front[n.name]; ok && f.Hash == hash {
			switch f.State {
			case StateOutOfDate:
				outOfDate++
				continue
			case StateResting:
				resting++
				continue
			case StatePutAway, StateWaiting:
				continue
			}
			e.uses = f.Uses
			if !f.At.IsZero() {
				e.mod = f.At
			}
			switch {
			case f.By == "person" || strings.HasPrefix(f.By, "person:"):
				e.group, e.label = 0, "(from a person)"
			case f.State == StateChecked:
				e.group, e.label = 1, "(checked)"
				if !f.At.IsZero() {
					e.label = "(checked " + f.At.UTC().Format("2006-01-02") + ")"
				}
			}
		}
		list = append(list, e)
	}
	slices.SortStableFunc(list, func(a, b entry) int {
		switch {
		case a.group != b.group:
			return a.group - b.group
		case a.uses != b.uses:
			return b.uses - a.uses
		}
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	var lines []string
	var used []fw.MemoryUsedRef
	size := 0
	for _, e := range list {
		line := indexLine(e.note, e.label)
		if len(lines) == IndexMaxNotes || (len(lines) > 0 && size+len(line)+1 > IndexMaxBytes) {
			break
		}
		lines = append(lines, line)
		used = append(used, fw.MemoryUsedRef{Ref: NoteRef(scope, e.name), Hash: e.hash})
		size += len(line) + 1
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Notes: %d kept, %.1f of %d KB shown.", len(notes), shownKB(size), IndexMaxBytes>>10)
	if !pulled.Answered {
		sb.WriteString("\n" + NoAnswerNote)
	}
	for _, l := range lines {
		sb.WriteString("\n" + l)
	}
	if left := leftOutLine(outOfDate, resting); left != "" {
		sb.WriteString("\n" + left)
	}
	writeMore(&sb, len(list)-len(lines), hint)
	if cut {
		sb.WriteString("\n" + CutNote)
	}
	return Brief{Text: sb.String(), Used: used}, nil
}

// leftOutLine counts the notes of the copy that the block leaves out
// because they are out of date or resting, such as "3 notes are out of
// date. View /memories to read them."
func leftOutLine(outOfDate, resting int) string {
	count := func(n int, what string) string {
		if n == 1 {
			return "1 note is " + what
		}
		return fmt.Sprintf("%d notes are %s", n, what)
	}
	var parts []string
	if outOfDate > 0 {
		parts = append(parts, count(outOfDate, "out of date"))
	}
	if resting > 0 {
		parts = append(parts, count(resting, "resting"))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", and ") + ". View " + Prefix + " to read them."
}

// shownKB is the size of the shown lines in KB, with one decimal. Any shown
// line counts as at least 0.1 KB, so the block never says 0.0 while it
// lists a note.
func shownKB(size int) float64 {
	kb := math.Ceil(float64(size)/102.4) / 10
	if size == 0 {
		return 0
	}
	return kb
}

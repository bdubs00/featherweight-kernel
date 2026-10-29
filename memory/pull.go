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
	"path"
	"slices"
	"strings"
	"sync"

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
// a note from before the front was set, goes to the front as a new note of
// the agent, with the sources that its lines name (textSources). When the
// front keeps another text under the same name, such as the note of
// another Task of the pool from before the front, the text of the copy
// goes to the front under a free name first (adopt), so no note from
// before the front is lost. The pull takes at most PullWait. A front that
// does not answer leaves the copy as it is. Without a front, or for the
// shared scope, it does nothing. A second pull sends nothing again.
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
	// added holds the notes that adopt sent under a free name.
	added := map[string]FrontNote{}
	// unchanged reports whether the note of the copy is as the pull saw it
	// before it took the lock: no write came between.
	unchanged := func(name string) bool {
		h, err := hashFile(files.Files, name)
		if prev, ok := before[name]; ok {
			return err == nil && h == prev
		}
		return errors.Is(err, fs.ErrNotExist)
	}
	// A note of the copy that the front never had, and that the front
	// lists under its name with another text, goes to the front under a
	// free name before the copy takes the text of the front or loses the
	// note. A note that the front did not take stays as it is, for the
	// next pull.
	hold := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(before)) {
		n, isListed := listed[name]
		if _, wasKnown := known[name]; wasKnown || !isListed || n.Hash == before[name] || !unchanged(name) {
			continue
		}
		if s.adopt(ctx, scope, files, &st, name, listed, before, added, &res) != nil {
			hold[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(fetched)) {
		rec := fetched[name]
		if hold[name] || !unchanged(name) {
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
		case hold[name]:
			continue
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
		links := s.linksOfCopy(ctx, &st, name, data)
		waiting, err := s.front.put(ctx, scope, name, frontPut{Text: string(data), IfHash: "absent", Sources: sourcesOf(links), By: "agent"})
		switch {
		case err == nil:
			n := FrontNote{Name: name, Hash: hash, By: "agent", Sources: len(links)}
			if waiting {
				n.State = StateWaiting
			}
			st.Front[name] = n
			setExtra(&st, name, noteExtra{Hash: hash, Links: links})
			res.Sent++
		case errors.Is(err, ErrFrontConflict):
			// The front has a note of that name that it does not list, such
			// as a note that it put away. The text of the copy goes to the
			// front under a free name, and the copy follows the front.
			if s.adopt(ctx, scope, files, &st, name, listed, before, added, &res) != nil {
				continue
			}
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
	maps.Copy(st.Front, added)
	_ = files.writeState(st)
	return res
}

// maxAdoptNames bounds the free names that adopt tries for one note.
const maxAdoptNames = 9

// adopt sends the text of the note name of the copy, which the front never
// had, to the front under a free name: name-2.md, name-3.md, and so on,
// a name that neither the list of the front, the copy, nor added holds.
// The copy keeps the text under the new name too, and added gets the
// note. A text that the front lists already, under any name, needs no new
// name. It returns an error when the front took the text under no name,
// and the caller then leaves the note of the copy as it is. The caller
// holds the lock of the scope.
func (s *Store) adopt(ctx context.Context, scope fw.MemoryScope, files *noteFiles, st *scopeState, name string,
	listed map[string]FrontNote, before map[string]string, added map[string]FrontNote, res *PullResult) error {
	hash := before[name]
	for _, n := range listed {
		if n.Hash == hash {
			return nil
		}
	}
	for _, n := range added {
		if n.Hash == hash {
			return nil
		}
	}
	data, err := fs.ReadFile(files.Files, name)
	if err != nil {
		return err
	}
	if hashOf(data) != hash {
		return errors.New("the note changed during the pull")
	}
	links := s.linksOfCopy(ctx, st, name, data)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; i <= maxAdoptNames+1; i++ {
		alt := fmt.Sprintf("%s-%d%s", stem, i, ext)
		_, onList := listed[alt]
		_, inCopy := before[alt]
		_, sent := added[alt]
		if onList || inCopy || sent || CheckName(alt) != nil {
			continue
		}
		waiting, err := s.front.put(ctx, scope, alt, frontPut{Text: string(data), IfHash: "absent", Sources: sourcesOf(links), By: "agent"})
		switch {
		case errors.Is(err, ErrFrontConflict):
			continue
		case err != nil:
			return err
		}
		n := FrontNote{Name: alt, Hash: hash, By: "agent", Sources: len(links)}
		if waiting {
			n.State = StateWaiting
		} else if writeAtomic(files.dir, alt, data) == nil {
			setExtra(st, alt, noteExtra{Hash: hash, Links: links})
		}
		added[alt] = n
		res.Sent++
		return nil
	}
	return fmt.Errorf("no free name for the note %s at the front", name)
}

// linksOfCopy returns the sources of the note name of the copy with the
// text data: those that the state keeps for that text, or else those
// that its lines name, with the version of each page.
func (s *Store) linksOfCopy(ctx context.Context, st *scopeState, name string, data []byte) []fw.MemoryLink {
	if e, ok := st.Notes[name]; ok && e.Hash == hashOf(data) && len(e.Links) > 0 {
		return e.Links
	}
	return s.textSources(ctx, string(data))
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

// pullFor takes the notes of the front into the copy for a run of scope:
// the notes of the scope, and the notes of the agent scope that a run of
// the principal scope also reads (AgentScope). It pulls both at once, so
// the start of a run waits PullWait at most. It reports whether the front
// answered every pull.
func (s *Store) pullFor(ctx context.Context, scope fw.MemoryScope) bool {
	agent, ok := AgentScope(scope)
	if !ok {
		return s.Pull(ctx, scope).Answered
	}
	var wg sync.WaitGroup
	var other PullResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		other = s.Pull(ctx, agent)
	}()
	own := s.Pull(ctx, scope)
	wg.Wait()
	return own.Answered && other.Answered
}

// frontGroup returns the notes of the copy of a scope for the block with
// a front, after the pull, in three ranks, each note with its label: notes
// that a person wrote ("(from a person)"), checked notes ("(checked
// <date>)"), and the other notes. Inside a rank, the notes that runs used
// most come first, then the newest. Notes that are out of date or resting
// stay out of the group, and the group counts them. The labels come from
// the last answer of the front.
func (s *Store) frontGroup(ctx context.Context, scope fw.MemoryScope, prefix string) (*briefGroup, error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, err
	}
	notes, cut, err := walkNotes(ctx, dir, -1)
	if err != nil {
		return nil, fmt.Errorf("memory: index of %s: %w", dir, err)
	}
	g := newGroup(scope, prefix)
	g.walked, g.cut = len(notes), cut
	if len(notes) == 0 {
		return g, nil
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return nil, err
	}
	st := readStateAt(dir)
	type entry struct {
		briefNote
		rank int
		uses int
	}
	var list []entry
	for _, n := range notes {
		hash := ""
		if c, ok := st.Hashes[n.name]; ok && c.Size == n.size && c.Mod == n.mod.UnixNano() && c.Ino == n.ino {
			hash = c.Hash
		} else if h, err := hashFile(files, n.name); err == nil {
			hash = h
		}
		e := entry{briefNote: briefNote{note: n, hash: hash}, rank: 2}
		if f, ok := st.Front[n.name]; ok && f.Hash == hash {
			switch f.State {
			case StateOutOfDate:
				g.outOfDate++
				continue
			case StateResting:
				g.resting++
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
				e.rank, e.label = 0, "(from a person)"
			case f.State == StateChecked:
				e.rank, e.label = 1, "(checked)"
				if !f.At.IsZero() {
					e.label = "(checked " + f.At.UTC().Format("2006-01-02") + ")"
				}
			}
		}
		list = append(list, e)
	}
	slices.SortStableFunc(list, func(a, b entry) int {
		switch {
		case a.rank != b.rank:
			return a.rank - b.rank
		case a.uses != b.uses:
			return b.uses - a.uses
		}
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	for _, e := range list {
		g.notes = append(g.notes, e.briefNote)
	}
	return g, nil
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

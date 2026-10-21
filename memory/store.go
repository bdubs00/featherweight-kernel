// Package memory holds the memory of the kernel: the notes that the memory
// tool keeps between runs, and the recall over notes, runs, workspace
// files, and skills. The memory and recall tools, the native API, and the
// hosts of harnesses each build a Store over the same directories, so every
// surface reads and writes the same notes with the same scope, limits, and
// audit. A lock on each scope directory orders the writes of every Store
// and every process.
//
// The notes are canonical Markdown files under <home>/memory. An optional
// SQLite FTS5 index, which the build tag memory_sqlite links, is derived
// from them and can be deleted at any time.
package memory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Limits of the notes of one scope.
const (
	MaxNoteBytes = 128 << 10 // the largest note
	MaxNotes     = 2000      // the most notes in one scope
	maxWalk      = 10000     // the most entries that one walk of a scope visits
)

// ErrLimit is the error of a write that a limit of the notes refuses: the
// size of a note (MaxNoteBytes) or the count of notes in a scope
// (MaxNotes). The message of the error says which, for a model to read.
var ErrLimit = errors.New("memory: a limit of the notes refuses the write")

// limitError is an error that is ErrLimit with a message for a model.
type limitError struct{ msg string }

func (e *limitError) Error() string        { return e.msg }
func (e *limitError) Is(target error) bool { return target == ErrLimit }

// Prefix is the path that a model uses for the notes of its scope.
const Prefix = "/memories"

// Store is the memory of one process. It is safe for concurrent use.
type Store struct {
	base  string // <home>/memory, absolute
	scope string // fw.MemoryScopeAgent, fw.MemoryScopePrincipal, or fw.MemoryScopeShared
	index noteIndex
}

var (
	_ fw.Memory       = (*Store)(nil)
	_ fw.MemoryWriter = (*Store)(nil)
)

// New returns the memory of the configuration: the home and the scope of
// tools.memory.
func New(cfg *fw.Config) (*Store, error) {
	home, scope := "", ""
	if cfg != nil {
		home, scope = cfg.Home, cfg.Tools.Memory.Scope
	}
	if home == "" {
		home = fw.DefaultHome()
	}
	switch scope = strings.ToLower(strings.TrimSpace(scope)); scope {
	case "":
		scope = fw.MemoryScopeAgent
	case fw.MemoryScopeAgent, fw.MemoryScopePrincipal, fw.MemoryScopeShared:
	default:
		return nil, fmt.Errorf("memory: tools.memory.scope is %q, want %s, %s, or %s", scope, fw.MemoryScopeAgent, fw.MemoryScopePrincipal, fw.MemoryScopeShared)
	}
	base, err := filepath.Abs(filepath.Join(home, "memory"))
	if err != nil {
		return nil, fmt.Errorf("memory: home %s: %w", home, err)
	}
	s := &Store{base: base, scope: scope}
	s.index = newNoteIndex(base)
	return s, nil
}

// Close closes the optional index.
func (s *Store) Close() error {
	if s.index == nil {
		return nil
	}
	return s.index.Close()
}

// Scope returns the scope of a run. The kernel builds the run info, so a
// tool or a caller cannot widen the scope. A run without an agent is the
// profile "default". In the principal scope, a run without a principal,
// such as a local run, uses the scope of its agent.
func (s *Store) Scope(info fw.RunInfo) fw.MemoryScope {
	agent := info.Agent
	if agent == "" {
		agent = "default"
	}
	sc := fw.MemoryScope{Kind: fw.MemoryAgentScope, Agent: agent, Principal: info.Principal, Task: info.RunID}
	switch s.scope {
	case fw.MemoryScopeShared:
		sc.Kind = fw.MemorySharedScope
	case fw.MemoryScopePrincipal:
		if info.Principal != "" {
			sc.Kind = fw.MemoryPrincipalScope
		}
	}
	return sc
}

// Dir returns the directory of the notes of a scope. It creates nothing.
func (s *Store) Dir(scope fw.MemoryScope) (string, error) {
	switch scope.Kind {
	case fw.MemorySharedScope:
		return filepath.Join(s.base, "shared"), nil
	case fw.MemoryPrincipalScope:
		if !ValidAgentName(scope.Agent) {
			return "", fmt.Errorf("the agent name %q cannot name a memory directory", scope.Agent)
		}
		if scope.Principal == "" {
			return "", errors.New("the principal scope needs a principal")
		}
		return filepath.Join(s.base, "principals", scope.Agent, principalKey(scope.Principal)), nil
	case fw.MemoryAgentScope:
		if !ValidAgentName(scope.Agent) {
			return "", fmt.Errorf("the agent name %q cannot name a memory directory", scope.Agent)
		}
		return filepath.Join(s.base, "agents", scope.Agent), nil
	}
	return "", fmt.Errorf("the memory scope %q is not agent, principal, or shared", scope.Kind)
}

// indexDir returns the directory of the derived index of a scope.
func (s *Store) indexDir(scope fw.MemoryScope) (string, error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(s.base, dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.base, "index", rel), nil
}

// principalKey names the directory of a principal. A hash keeps the
// subject, which can be an email address, out of the path.
func principalKey(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:16])
}

// ValidAgentName applies the rule of profile names: letters, digits, '-',
// '_', and '.', at most 64 bytes, and no leading '.'. Such a name is one
// path element inside the memory directory.
func ValidAgentName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '.' {
		return false
	}
	for _, c := range name {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}

// CheckName checks the name of a note: an io/fs name with no element that
// starts with '.'. A hidden note persists outside the index and the
// listings, so the store refuses it.
func CheckName(name string) error {
	if !fs.ValidPath(name) || name == "." {
		return fmt.Errorf("the path %q is not a note", name)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("the path %q has a hidden name; a note name must not start with '.'", name)
		}
	}
	return nil
}

// NoteRef returns the ref of a note of a scope. The ref names the kind of
// the scope and the agent, never the principal: Get resolves it in the
// scope of the caller.
func NoteRef(scope fw.MemoryScope, name string) string {
	switch scope.Kind {
	case fw.MemorySharedScope:
		return "mem://shared/" + name
	case fw.MemoryPrincipalScope:
		return "mem://principal/" + scope.Agent + "/" + name
	}
	return "mem://agent/" + scope.Agent + "/" + name
}

// noteName returns the name of the note that a ref names. A ref of another
// scope or another agent is an error, so a caller cannot read outside its
// scope by writing a ref.
func noteName(scope fw.MemoryScope, ref string) (string, error) {
	rest, ok := strings.CutPrefix(ref, "mem://")
	if !ok {
		return "", fmt.Errorf("the ref %q is not a note", ref)
	}
	var kind, agent, name string
	if r, ok := strings.CutPrefix(rest, "shared/"); ok {
		kind, name = string(fw.MemorySharedScope), r
	} else {
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 {
			return "", fmt.Errorf("the ref %q is not mem://<scope>/<agent>/<path>", ref)
		}
		kind, agent, name = parts[0], parts[1], parts[2]
	}
	if fw.MemoryScopeKind(kind) != scope.Kind || (scope.Kind != fw.MemorySharedScope && agent != scope.Agent) {
		return "", fmt.Errorf("the ref %q is outside the memory of this run", ref)
	}
	name = path.Clean(name)
	if err := CheckName(name); err != nil {
		return "", fmt.Errorf("the ref %q leaves the memory directory or names a hidden note", ref)
	}
	return name, nil
}

// Notes returns the notes of a scope as a store for the text editor of the
// memory tool. It creates the private directory of the scope. Each write
// is atomic, keeps the limits of a note and of a scope, and records
// fw.memory.changed in the ledger of the tool call in ctx.
func (s *Store) Notes(ctx context.Context, scope fw.MemoryScope) (fw.Files, error) {
	return s.notes(ctx, scope)
}

func (s *Store) notes(ctx context.Context, scope fw.MemoryScope) (*noteFiles, error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%s cannot be created: %w", Prefix, err)
	}
	inner, err := fw.DirFiles(dir)
	if err != nil {
		return nil, err
	}
	return &noteFiles{Files: inner, dir: dir, ctx: ctx, scope: scope}, nil
}

// noteFiles is the store of the notes of one scope for one call.
type noteFiles struct {
	fw.Files
	dir   string
	ctx   context.Context
	scope fw.MemoryScope
}

// WriteFile implements fw.Files. The write goes to a hidden temporary file
// that is synced and then renamed over the note, so a stop of the process
// leaves the old note or the new one, never a part.
func (f *noteFiles) WriteFile(name string, data []byte) error {
	unlock, err := lockDir(f.dir)
	if err != nil {
		return err
	}
	defer unlock()
	return f.writeLocked(name, data)
}

// writeLocked is WriteFile for a caller that holds the lock of the scope.
func (f *noteFiles) writeLocked(name string, data []byte) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if len(data) > MaxNoteBytes {
		return &limitError{fmt.Sprintf("a note holds at most %d KB, and this text has %d KB", MaxNoteBytes>>10, (len(data)+1023)>>10)}
	}
	op := fw.FileChanged
	if _, err := f.Files.Stat(name); errors.Is(err, fs.ErrNotExist) {
		op = fw.FileCreated
		if n := countNotes(f.Files); n >= MaxNotes {
			return &limitError{fmt.Sprintf("the memory holds %d notes, the most that one scope holds; update, merge, or delete a note first", n)}
		}
	}
	if err := writeAtomic(f.dir, name, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:     op,
		Ref:    NoteRef(f.scope, name),
		Scope:  f.scope.Kind,
		Agent:  f.scope.Agent,
		Size:   int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]),
	})
	return nil
}

// Remove implements fw.Files.
func (f *noteFiles) Remove(name string) error {
	unlock, err := lockDir(f.dir)
	if err != nil {
		return err
	}
	defer unlock()
	return f.removeLocked(name)
}

// removeLocked is Remove for a caller that holds the lock of the scope.
func (f *noteFiles) removeLocked(name string) error {
	if err := f.Files.Remove(name); err != nil {
		return err
	}
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:    fw.FileRemoved,
		Ref:   NoteRef(f.scope, name),
		Scope: f.scope.Kind,
		Agent: f.scope.Agent,
	})
	return nil
}

// Rename implements fw.Files.
func (f *noteFiles) Rename(oldname, newname string) error {
	if err := CheckName(newname); err != nil {
		return err
	}
	unlock, err := lockDir(f.dir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := f.Files.Rename(oldname, newname); err != nil {
		return err
	}
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:     fw.FileRenamed,
		Ref:    NoteRef(f.scope, newname),
		OldRef: NoteRef(f.scope, oldname),
		Scope:  f.scope.Kind,
		Agent:  f.scope.Agent,
	})
	return nil
}

// writeAtomic writes data to name under dir through os.Root: a hidden
// temporary file, a sync, and a rename over the name.
func writeAtomic(dir, name string, data []byte) error {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	local := filepath.FromSlash(name)
	parent := filepath.Dir(local)
	if parent != "." {
		if err := r.MkdirAll(parent, 0o700); err != nil {
			return err
		}
	}
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(parent, "."+filepath.Base(local)+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = r.Rename(tmp, local)
	}
	if werr != nil {
		_ = r.Remove(tmp)
	}
	return werr
}

// countNotes counts the notes of a store, up to MaxNotes.
func countNotes(files fs.FS) int {
	n, seen := 0, 0
	_ = fs.WalkDir(files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		seen++
		if seen > maxWalk || n >= MaxNotes {
			return fs.SkipAll
		}
		if name != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

// Remember writes the note that rec.Ref names with rec.Body. A non-empty
// ifHash makes the write a compare-and-swap: it fails with
// fw.ErrMemoryConflict unless the current note has that hash. The hash
// "absent" requires that no note exists, and "present" that one exists. The
// lock of the scope holds from the read to the answer, so two writers with
// the same hash never both succeed. It returns the note as Get does.
func (s *Store) Remember(ctx context.Context, scope fw.MemoryScope, rec fw.MemoryRecord, ifHash string) (fw.MemoryRecord, error) {
	name, err := noteName(scope, rec.Ref)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	files, err := s.notes(ctx, scope)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	unlock, err := lockDir(files.dir)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	defer unlock()
	if ifHash != "" {
		cur, err := fs.ReadFile(files, name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if ifHash != "absent" {
				return fw.MemoryRecord{}, fw.ErrMemoryConflict
			}
		case err != nil:
			return fw.MemoryRecord{}, err
		case ifHash == "present":
		default:
			if hashOf(cur) != ifHash {
				return fw.MemoryRecord{}, fw.ErrMemoryConflict
			}
		}
	}
	if err := files.writeLocked(name, []byte(rec.Body)); err != nil {
		return fw.MemoryRecord{}, err
	}
	return s.getNote(scope, rec.Ref)
}

// Forget removes the note that ref names. It refuses a directory, so each
// note that goes has its own record.
func (s *Store) Forget(ctx context.Context, scope fw.MemoryScope, ref string) error {
	name, err := noteName(scope, ref)
	if err != nil {
		return err
	}
	files, err := s.notes(ctx, scope)
	if err != nil {
		return err
	}
	unlock, err := lockDir(files.dir)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := files.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fw.ErrMemoryNotFound
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory, not a note; forget its notes one at a time", fw.ErrInvalidRequest, ref)
	}
	return files.removeLocked(name)
}

// List returns the notes of a scope, newest first, without their bodies.
func (s *Store) List(ctx context.Context, scope fw.MemoryScope) ([]fw.MemoryRecord, error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, err
	}
	notes, err := walkNotes(ctx, dir, -1)
	if err != nil {
		return nil, err
	}
	out := make([]fw.MemoryRecord, 0, len(notes))
	for _, n := range notes {
		out = append(out, fw.MemoryRecord{
			Ref:         NoteRef(scope, n.name),
			Kind:        fw.MemoryKindNote,
			Scope:       scope,
			Title:       n.title,
			Description: n.title,
			Source:      "memory",
			Trust:       "untrusted",
			Size:        n.size,
			Updated:     n.mod,
		})
	}
	return out, nil
}

// getNote reads one note with its hash.
func (s *Store) getNote(scope fw.MemoryScope, ref string) (fw.MemoryRecord, error) {
	name, err := noteName(scope, ref)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	dir, err := s.Dir(scope)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	info, err := files.Stat(name)
	if err != nil {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	if info.IsDir() {
		return fw.MemoryRecord{}, fmt.Errorf("%s is a directory, not a note", ref)
	}
	f, err := files.Open(name)
	if err != nil {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	body, err := io.ReadAll(io.LimitReader(f, MaxNoteBytes+1))
	f.Close()
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	title := firstLine(files, name)
	return fw.MemoryRecord{
		Ref:         NoteRef(scope, name),
		Kind:        fw.MemoryKindNote,
		Scope:       scope,
		Title:       title,
		Description: title,
		Body:        string(body),
		Source:      "memory",
		Trust:       "untrusted",
		Size:        info.Size(),
		Hash:        hashOf(body),
		Updated:     info.ModTime(),
	}, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

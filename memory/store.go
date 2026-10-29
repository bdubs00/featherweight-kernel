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
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Limits of the notes of one scope.
const (
	MaxNoteBytes = 128 << 10 // the largest note
	MaxNotes     = 2000      // the most notes in one scope
	MaxNoteDepth = 8         // the most parts of a note name: a/b/c.md has 3
	// maxWalk bounds one walk of a scope: it visits at most this many
	// notes, and apart from them at most this many other entries, such as
	// directories and hidden files.
	maxWalk = 10000
)

// Bounds of the lock of a scope.
const (
	lockWait    = 30 * time.Second      // the longest wait for the lock
	lockPollMax = 50 * time.Millisecond // the longest pause between two tries
	staleTemp   = time.Minute           // the age at which a sweep without flock removes a temporary file
)

// ErrLimit is the error of a write that a limit of the notes refuses: the
// size of a note (MaxNoteBytes), the count of notes in a scope (MaxNotes),
// or a scope directory that holds too many other entries to count its
// notes. The message of the error says which, for a model to read.
var ErrLimit = errors.New("memory: a limit of the notes refuses the write")

// ErrLockTimeout is the error of a write that waited 30 seconds for the
// lock of its scope while another writer held it.
var ErrLockTimeout = errors.New("memory: the lock of the scope stayed busy")

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
	swept sync.Map // the scope directories that this Store swept of temporary files
	// front keeps the notes of the scopes agent and principal when
	// tools.memory.front is set, or is nil (front.go).
	front *front
	// runs holds the runs of the archive of the front that recall found,
	// so a ref of one of them reads it (recall.go).
	runs runCache
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
	var fc *fw.MemoryFrontConfig
	if cfg != nil {
		fc = cfg.Tools.Memory.Front
	}
	f, err := newFront(fc)
	if err != nil {
		return nil, err
	}
	s := &Store{base: base, scope: scope, front: f}
	s.index = newNoteIndex(base)
	return s, nil
}

// Front reports whether the front of a fleet keeps the notes of a scope,
// and the files of the scope are a copy (docs/KNOWHOW.md section 4.2).
func (s *Store) Front(scope fw.MemoryScope) bool { return s.front != nil && frontScope(scope) }

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
// such as a job that a publisher sent without one, uses the scope of its
// agent.
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

// CheckName checks the name of a note: an io/fs name in UTF-8, with at
// most MaxNoteDepth parts, no part that starts with '.', and no control
// character (a rune below 0x20, or 0x7f). A hidden note persists outside
// the index and the listings, and a newline in a name forges a line of the
// index, so the store refuses both.
func CheckName(name string) error {
	if !fs.ValidPath(name) || name == "." {
		return fmt.Errorf("the path %q is not a note", name)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("the path %q is not valid UTF-8", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("the path %q holds a control character; a note name holds none", name)
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) > MaxNoteDepth {
		return fmt.Errorf("the path %q has %d parts; a note name has at most %d", name, len(parts), MaxNoteDepth)
	}
	for _, part := range parts {
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
// fw.memory.changed in the ledger of the tool call in ctx. Each write takes
// the lock of the scope on its own; Locked holds it across a read and the
// write that follows.
func (s *Store) Notes(ctx context.Context, scope fw.MemoryScope) (fw.Files, error) {
	return s.notes(ctx, scope)
}

// Locked runs fn with the notes of a scope while it holds the lock of the
// scope, so a read in fn and the write that follows it are one step: no
// write of another Store or another process comes between them. The files
// that fn gets write, remove, and rename as Notes does, without taking the
// lock again. The memory tool runs each command that writes through it, so
// an edit never loses a concurrent write. After fn returns, the files take
// the lock again for each write.
func (s *Store) Locked(ctx context.Context, scope fw.MemoryScope, fn func(files fw.Files) error) error {
	files, err := s.notes(ctx, scope)
	if err != nil {
		return err
	}
	unlock, err := lockDir(ctx, files.dir)
	if err != nil {
		return err
	}
	defer unlock()
	files.locked = true
	defer func() { files.locked = false }()
	return fn(files)
}

// Edit changes the note that ref names while it holds the lock of the
// scope from the read to the write: fn gets the text of the note and
// returns the new text. A missing note is fw.ErrMemoryNotFound, and a
// directory is fw.ErrInvalidRequest. An error of fn leaves the note as it
// is and returns that error. Edit returns the note as Get does.
func (s *Store) Edit(ctx context.Context, scope fw.MemoryScope, ref string, fn func(old []byte) ([]byte, error)) (fw.MemoryRecord, error) {
	name, err := noteName(scope, ref)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	err = s.Locked(ctx, scope, func(files fw.Files) error {
		info, err := files.Stat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fw.ErrMemoryNotFound
		case err != nil:
			return err
		case info.IsDir():
			return fmt.Errorf("%w: %s is a directory, not a note", fw.ErrInvalidRequest, ref)
		}
		old, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		next, err := fn(old)
		if err != nil {
			return err
		}
		return files.WriteFile(name, next)
	})
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	return s.getNote(scope, ref)
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
	if _, done := s.swept.LoadOrStore(dir, true); !done {
		s.sweep(ctx, dir)
	}
	return &noteFiles{Files: inner, dir: dir, ctx: ctx, scope: scope, store: s}, nil
}

// sweep removes the hidden temporary files of a scope that a write left
// when its process stopped between the create and the rename. It holds the
// lock of the scope, so no write of any Store or process is between the
// two steps. On a system without flock it removes only a file older than
// staleTemp. A file that stays hides from every listing, and the next
// Store sweeps again.
func (s *Store) sweep(ctx context.Context, dir string) {
	unlock, err := lockDir(ctx, dir)
	if err != nil {
		s.swept.Delete(dir)
		return
	}
	defer unlock()
	r, err := os.OpenRoot(dir)
	if err != nil {
		return
	}
	defer r.Close()
	seen := 0
	_ = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil
		}
		if seen++; seen > 2*maxWalk {
			return fs.SkipAll
		}
		if p == "." {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || pathParts(p) >= MaxNoteDepth {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !isTempName(d.Name()) {
			return nil
		}
		if !lockHeld {
			if info, err := d.Info(); err != nil || time.Since(info.ModTime()) < staleTemp {
				return nil
			}
		}
		_ = r.Remove(filepath.FromSlash(p))
		return nil
	})
}

// tempMark is the part of the name of a temporary file of writeAtomic that
// comes before its 12 hex digits.
const tempMark = ".tmp-"

// isTempName reports whether a base name is one of a temporary file of
// writeAtomic: ".<note>.tmp-<12 hex digits>".
func isTempName(base string) bool {
	i := strings.LastIndex(base, tempMark)
	if !strings.HasPrefix(base, ".") || i < 1 {
		return false
	}
	hexPart := base[i+len(tempMark):]
	if len(hexPart) != 12 {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

// pathParts counts the parts of a slash-separated path.
func pathParts(p string) int { return strings.Count(p, "/") + 1 }

// noteFiles is the store of the notes of one scope for one call.
type noteFiles struct {
	fw.Files
	dir    string
	ctx    context.Context
	scope  fw.MemoryScope
	store  *Store
	locked bool // the caller holds the lock of the scope (Locked)
	// links and meta are the sources and the fields of the record of a
	// write through Remember, which the note keeps with its text.
	links []fw.MemoryLink
	meta  []fw.MemoryField
}

// pushes reports whether each change of these notes goes to the front.
func (f *noteFiles) pushes() bool { return f.store != nil && f.store.Front(f.scope) }

// lock takes the lock of the scope, unless the caller holds it.
func (f *noteFiles) lock() (func(), error) {
	if f.locked {
		return func() {}, nil
	}
	return lockDir(f.ctx, f.dir)
}

// WriteFile implements fw.Files. The write goes to a hidden temporary file
// that is synced and then renamed over the note, so a stop of the process
// leaves the old note or the new one, never a part.
func (f *noteFiles) WriteFile(name string, data []byte) error {
	unlock, err := f.lock()
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
	var old []byte
	existed := false
	if info, err := f.Files.Stat(name); errors.Is(err, fs.ErrNotExist) {
		op = fw.FileCreated
		n, cut, err := countNotes(f.ctx, f.Files)
		switch {
		case err != nil:
			return err
		case cut:
			// The count did not finish, so the store cannot tell whether a
			// new note keeps the limit, and it refuses the note.
			return &limitError{fmt.Sprintf("the memory directory holds more than %d directories and other files that are not notes, so the store cannot count the notes and refuses a new one; remove what is not a note first", maxWalk)}
		case n >= MaxNotes:
			return &limitError{fmt.Sprintf("the memory holds %d notes, the most that one scope holds; update, merge, or delete a note first", n)}
		}
	} else if err == nil && !info.IsDir() && f.pushes() {
		if old, err = fs.ReadFile(f.Files, name); err != nil {
			return err
		}
		existed = true
	}
	if err := writeAtomic(f.dir, name, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	// A write without sources of its own takes those that the text names
	// (SourcesOf), and each page gets its version from the front.
	links := f.links
	if links == nil {
		links = SourcesOf(string(data))
	}
	links = f.store.versioned(f.ctx, links)
	waiting := false
	if f.pushes() {
		var err error
		if waiting, err = f.push(name, data, hash, old, existed, links); err != nil {
			return err
		}
	}
	f.keepExtra(name, hash, links)
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:      op,
		Ref:     NoteRef(f.scope, name),
		Scope:   f.scope.Kind,
		Agent:   f.scope.Agent,
		Size:    int64(len(data)),
		SHA256:  hash,
		Sources: links,
		Waiting: waiting,
	})
	return nil
}

// Remove implements fw.Files.
func (f *noteFiles) Remove(name string) error {
	unlock, err := f.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return f.removeLocked(name)
}

// removeLocked is Remove for a caller that holds the lock of the scope. It
// also removes the rows of the derived index, so the text of the note
// leaves the index at once and not at the next search. With a front, it
// removes one note, never a directory, and the front puts the note away
// (pushRemove).
func (f *noteFiles) removeLocked(name string) error {
	var old []byte
	if f.pushes() {
		info, err := f.Files.Stat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errDirAtFront
		}
		if old, err = fs.ReadFile(f.Files, name); err != nil {
			return err
		}
	}
	if err := f.Files.Remove(name); err != nil {
		return err
	}
	if err := syncParent(f.dir, name); err != nil {
		return err
	}
	waiting := false
	if f.pushes() {
		var err error
		if waiting, err = f.pushRemove(name, old); err != nil {
			return err
		}
	}
	f.dropExtra(name)
	f.store.dropFromIndex(f.ctx, f.scope, name)
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:      fw.FileRemoved,
		Ref:     NoteRef(f.scope, name),
		Scope:   f.scope.Kind,
		Agent:   f.scope.Agent,
		Waiting: waiting,
	})
	return nil
}

// Rename implements fw.Files. With a front, it renames one note, never a
// directory, and the front gets the note under the new name and puts the
// old name away (pushRename).
func (f *noteFiles) Rename(oldname, newname string) error {
	if err := CheckName(newname); err != nil {
		return err
	}
	unlock, err := f.lock()
	if err != nil {
		return err
	}
	defer unlock()
	var data []byte
	if f.pushes() {
		info, err := f.Files.Stat(oldname)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errDirAtFront
		}
		if _, err := f.Files.Stat(newname); err == nil {
			return &fs.PathError{Op: "rename", Path: newname, Err: fs.ErrExist}
		}
		if data, err = fs.ReadFile(f.Files, oldname); err != nil {
			return err
		}
	}
	if err := f.Files.Rename(oldname, newname); err != nil {
		return err
	}
	if err := syncParent(f.dir, oldname); err != nil {
		return err
	}
	if err := syncParent(f.dir, newname); err != nil {
		return err
	}
	waiting := false
	if f.pushes() {
		var err error
		if waiting, err = f.pushRename(oldname, newname, data); err != nil {
			return err
		}
	}
	f.moveExtra(oldname, newname)
	// The next search indexes the text under the new name.
	f.store.dropFromIndex(f.ctx, f.scope, oldname)
	fw.RecordMemoryChange(f.ctx, fw.MemoryChangedData{
		Op:      fw.FileRenamed,
		Ref:     NoteRef(f.scope, newname),
		OldRef:  NoteRef(f.scope, oldname),
		Scope:   f.scope.Kind,
		Agent:   f.scope.Agent,
		Waiting: waiting,
	})
	return nil
}

// dropFromIndex removes a note, or every note under a directory, from the
// derived index of a scope, when the build has an index and the index of
// the scope exists. A failure leaves the rows to the next search, which
// removes the rows of every note that is gone.
func (s *Store) dropFromIndex(ctx context.Context, scope fw.MemoryScope, name string) {
	if s.index == nil {
		return
	}
	dir, err := s.indexDir(scope)
	if err != nil {
		return
	}
	_ = s.index.Remove(ctx, dir, name)
}

// writeAtomic writes data to name under dir through os.Root: a hidden
// temporary file, a sync, a rename over the name, and a sync of the
// directory that holds the name, so the rename lasts through a stop of the
// system too.
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
	tmp := filepath.Join(parent, "."+filepath.Base(local)+tempMark+hex.EncodeToString(suffix[:]))
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
		return werr
	}
	return syncDir(r, parent)
}

// syncParent syncs the directory that holds name under dir.
func syncParent(dir, name string) error {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	return syncDir(r, filepath.Dir(filepath.FromSlash(name)))
}

// syncDir syncs a directory of r, so a rename or a removal in it lasts. A
// file system that cannot sync a directory is no error.
func syncDir(r *os.Root, name string) error {
	d, err := r.Open(name)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, errors.ErrUnsupported) {
		return nil
	}
	return err
}

// countNotes counts the notes of a store, up to MaxNotes. cut reports a
// walk that stopped at maxWalk other entries before it counted every note,
// so the count is too low and a write must not trust it.
func countNotes(ctx context.Context, files fs.FS) (n int, cut bool, err error) {
	cut, err = walkScope(ctx, files, MaxNotes, func(string, fs.DirEntry) { n++ })
	return n, cut, err
}

// walkScope calls fn for each note of fsys, in lexical order: each regular
// file whose path has no hidden part and at most MaxNoteDepth parts. It
// skips hidden directories and the directories that are too deep to hold a
// note. It stops after limit notes, or after maxWalk other entries; the
// second stop is a cut, because notes can remain. An unreadable entry
// leaves the walk, and an unreadable root is an error.
func walkScope(ctx context.Context, fsys fs.FS, limit int, fn func(p string, d fs.DirEntry)) (cut bool, err error) {
	notes, other := 0, 0
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			if other++; other > maxWalk {
				cut = true
				return fs.SkipAll
			}
			if d.IsDir() && (strings.HasPrefix(d.Name(), ".") || pathParts(p) >= MaxNoteDepth) {
				return fs.SkipDir
			}
			return nil
		}
		if notes >= limit {
			if limit >= maxWalk {
				cut = true
			}
			return fs.SkipAll
		}
		notes++
		fn(p, d)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return cut, err
}

// Remember writes the note that rec.Ref names with rec.Body. A non-empty
// ifHash makes the write a compare-and-swap: it fails with
// fw.ErrMemoryConflict unless the current note has that hash. The hash
// "absent" requires that no note exists, and "present" that one exists. The
// lock of the scope holds from the read to the answer, so two writers with
// the same hash never both succeed. The note keeps rec.Links as its
// sources and rec.Metadata as its fields, for this text: Get returns them,
// and a front gets the links as the sources of the note. It returns the
// note as Get does.
func (s *Store) Remember(ctx context.Context, scope fw.MemoryScope, rec fw.MemoryRecord, ifHash string) (fw.MemoryRecord, error) {
	name, err := noteName(scope, rec.Ref)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	files, err := s.notes(ctx, scope)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	unlock, err := lockDir(ctx, files.dir)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	defer unlock()
	info, err := files.Stat(name)
	exists := err == nil
	switch {
	case exists && info.IsDir():
		return fw.MemoryRecord{}, fmt.Errorf("%w: %s is a directory, not a note", fw.ErrInvalidRequest, rec.Ref)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fw.MemoryRecord{}, err
	}
	switch {
	case ifHash == "":
	case !exists:
		if ifHash != "absent" {
			return fw.MemoryRecord{}, fw.ErrMemoryConflict
		}
	case ifHash == "present":
	case ifHash == "absent":
		return fw.MemoryRecord{}, fw.ErrMemoryConflict
	default:
		sum, err := hashFile(files, name)
		if err != nil {
			return fw.MemoryRecord{}, err
		}
		if sum != ifHash {
			return fw.MemoryRecord{}, fw.ErrMemoryConflict
		}
	}
	files.links, files.meta = rec.Links, rec.Metadata
	if err := files.writeLocked(name, []byte(rec.Body)); err != nil {
		return fw.MemoryRecord{}, err
	}
	return s.getNote(scope, rec.Ref)
}

// Forget removes the note that ref names. It refuses a directory, so each
// note that goes has its own record.
func (s *Store) Forget(ctx context.Context, scope fw.MemoryScope, ref string) error {
	_, err := s.ForgetNote(ctx, scope, ref)
	return err
}

// ForgetNote is Forget that also returns the note that it removed, without
// its text: the ref, the size, the hash, and the time of the last change,
// so a caller that logs the removal, such as the native API, can name
// them.
func (s *Store) ForgetNote(ctx context.Context, scope fw.MemoryScope, ref string) (fw.MemoryRecord, error) {
	name, err := noteName(scope, ref)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	files, err := s.notes(ctx, scope)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	unlock, err := lockDir(ctx, files.dir)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	defer unlock()
	info, err := files.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	if info.IsDir() {
		return fw.MemoryRecord{}, fmt.Errorf("%w: %s is a directory, not a note; forget its notes one at a time", fw.ErrInvalidRequest, ref)
	}
	sum, err := hashFile(files, name)
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	gone := fw.MemoryRecord{
		Ref:     NoteRef(scope, name),
		Kind:    fw.MemoryKindNote,
		Scope:   scope,
		Source:  "memory",
		Trust:   "untrusted",
		Size:    info.Size(),
		Hash:    sum,
		Updated: info.ModTime(),
	}
	if err := files.removeLocked(name); err != nil {
		return fw.MemoryRecord{}, err
	}
	return gone, nil
}

// List returns the notes of a scope, newest first, without their bodies.
// cut reports a walk that stopped at its bound, so notes can be missing
// from the list (CutNote).
func (s *Store) List(ctx context.Context, scope fw.MemoryScope) (_ []fw.MemoryRecord, cut bool, err error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, false, err
	}
	notes, cut, err := walkNotes(ctx, dir, -1)
	if err != nil {
		return nil, false, err
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
	return out, cut, nil
}

// PrincipalNotes counts the notes of the principal scope of an agent:
// the people who have a directory there, and their notes together. The
// directories hold a hash of each subject, so the count names nobody. cut
// reports a walk of a person that stopped at its bound, so the count of
// the notes is too low.
func (s *Store) PrincipalNotes(ctx context.Context, agent string) (people, notes int, cut bool, err error) {
	if !ValidAgentName(agent) {
		return 0, 0, false, fmt.Errorf("memory: the agent name %q cannot name a memory directory", agent)
	}
	dir := filepath.Join(s.base, "principals", agent)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("memory: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		list, c, err := walkNotes(ctx, filepath.Join(dir, e.Name()), 0)
		if err != nil {
			return 0, 0, false, fmt.Errorf("memory: %w", err)
		}
		people++
		notes += len(list)
		cut = cut || c
	}
	return people, notes, cut, nil
}

// getNote reads one note with its hash. The hash covers the whole file,
// as the hash of Remember does; the body holds at most MaxNoteBytes.
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
		return fw.MemoryRecord{}, fmt.Errorf("%w: %s is a directory, not a note", fw.ErrInvalidRequest, ref)
	}
	f, err := files.Open(name)
	if err != nil {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	h := sha256.New()
	body, err := io.ReadAll(io.LimitReader(io.TeeReader(f, h), MaxNoteBytes))
	if err == nil {
		_, err = io.Copy(h, f)
	}
	f.Close()
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	title := firstLine(files, name)
	rec := fw.MemoryRecord{
		Ref:         NoteRef(scope, name),
		Kind:        fw.MemoryKindNote,
		Scope:       scope,
		Title:       title,
		Description: title,
		Body:        string(body),
		Source:      "memory",
		Trust:       "untrusted",
		Size:        info.Size(),
		Hash:        hex.EncodeToString(h.Sum(nil)),
		Updated:     info.ModTime(),
	}
	if e, ok := readStateAt(dir).Notes[name]; ok && e.Hash == rec.Hash {
		rec.Links, rec.Metadata = e.Links, e.Metadata
	}
	return rec, nil
}

// hashFile returns the hex SHA-256 of the whole file name.
func hashFile(files fs.FS, name string) (string, error) {
	f, err := files.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

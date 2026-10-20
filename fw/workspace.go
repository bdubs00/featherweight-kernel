package fw

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultMaxFileBytes is the default of tools.files.maxFileBytes.
const DefaultMaxFileBytes = 8 << 20

// workspace is the kernel side of Files (section 7.9). It checks the real
// name of each operation against the reserved paths and the rules of
// tools.files, and it records each change of a tool call.
type workspace struct {
	inner    Files
	reserved []string // real names that hold the state of fw; "." when the root lies under one
	deny     []string
	readOnly []string
	max      int64
	emit     func(FileChangedData) // nil outside a tool call
	// prefix is the directory of inner under the workspace of the agent,
	// such as runs/<id> for the directory of a run (sub), or "". A change
	// records its path under the workspace of the agent.
	prefix string
}

var _ Files = (*workspace)(nil)

// NewWorkspace wraps a store with the rules of tools.files and the
// reserved paths of cfg. The agent does this for its workspace. A tool
// uses it for a call outside an agent, such as in a test.
func NewWorkspace(f Files, cfg *Config) (Files, error) {
	w, err := newWorkspace(f, cfg)
	if err != nil {
		return nil, err
	}
	return w, nil
}

// newWorkspace wraps a store with the rules of cfg.
func newWorkspace(inner Files, cfg *Config) (*workspace, error) {
	fc := cfg.Tools.Files
	for _, list := range [][]string{fc.Deny, fc.ReadOnly} {
		for _, p := range list {
			if _, err := path.Match(strings.Trim(p, "/"), ""); err != nil {
				return nil, fmt.Errorf("fw: tools.files: the pattern %q: %w", p, err)
			}
		}
	}
	w := &workspace{inner: inner, deny: fc.Deny, readOnly: fc.ReadOnly, max: fc.MaxFileBytes}
	if w.max <= 0 {
		w.max = DefaultMaxFileBytes
	}
	d, ok := inner.(interface{ Dir() string })
	if !ok {
		return w, nil
	}
	root := evalExisting(d.Dir())
	for _, p := range cfg.Reserved() {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("fw: reserved path %s: %w", p, err)
		}
		r := evalExisting(abs)
		if rel, err := filepath.Rel(root, r); err == nil && filepath.IsLocal(rel) {
			w.reserved = append(w.reserved, filepath.ToSlash(rel))
		} else if rel, err := filepath.Rel(r, root); err == nil && filepath.IsLocal(rel) {
			w.reserved = append(w.reserved, ".")
		}
	}
	return w, nil
}

// bind returns the workspace of one tool call, which passes each change
// to emit.
func (w *workspace) bind(emit func(FileChangedData)) *workspace {
	c := *w
	c.emit = emit
	return &c
}

// foldCase is true where file systems ignore case by default.
var foldCase = runtime.GOOS == "darwin" || runtime.GOOS == "windows"

func (w *workspace) isReserved(real string) bool {
	for _, r := range w.reserved {
		if r == "." {
			return true
		}
		name := real
		if foldCase {
			r, name = strings.ToLower(r), strings.ToLower(real)
		}
		if name == r || strings.HasPrefix(name, r+"/") {
			return true
		}
	}
	return false
}

// RealName returns name as the store resolves it.
func (w *workspace) RealName(name string) (string, error) { return RealName(w.inner, name) }

// check resolves name and applies the rules. write selects the rules of a
// change. It returns the real name.
func (w *workspace) check(op, name string, write bool) (string, error) {
	real, err := RealName(w.inner, name)
	if err != nil {
		return "", err
	}
	switch {
	case w.isReserved(real):
		return "", &fs.PathError{Op: op, Path: name, Err: ErrFileReserved}
	case matchAny(w.deny, real):
		return "", &fs.PathError{Op: op, Path: name, Err: ErrFileDenied}
	case write && matchAny(w.readOnly, real):
		return "", &fs.PathError{Op: op, Path: name, Err: ErrFileReadOnly}
	}
	return real, nil
}

// Open implements fs.FS. A regular file larger than the limit fails.
func (w *workspace) Open(name string) (fs.File, error) {
	real, err := w.check("open", name, false)
	if err != nil {
		return nil, err
	}
	f, err := w.inner.Open(real)
	if err != nil {
		return nil, err
	}
	if info, err := f.Stat(); err == nil && info.Mode().IsRegular() && info.Size() > w.max {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: ErrFileTooLarge}
	}
	return f, nil
}

// Stat implements fs.StatFS.
func (w *workspace) Stat(name string) (fs.FileInfo, error) {
	real, err := w.check("stat", name, false)
	if err != nil {
		return nil, err
	}
	return w.inner.Stat(real)
}

// ReadDir implements fs.ReadDirFS. It leaves out each entry that the
// rules refuse.
func (w *workspace) ReadDir(name string) ([]fs.DirEntry, error) {
	real, err := w.check("readdir", name, false)
	if err != nil {
		return nil, err
	}
	entries, err := w.inner.ReadDir(real)
	out := entries[:0]
	for _, e := range entries {
		child := path.Join(real, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			if child, err = RealName(w.inner, child); err != nil {
				continue
			}
		}
		if !w.isReserved(child) && !matchAny(w.deny, child) {
			out = append(out, e)
		}
	}
	return out, err
}

// WriteFile implements Files.
func (w *workspace) WriteFile(name string, data []byte) error {
	real, err := w.check("write", name, true)
	if err != nil {
		return err
	}
	if int64(len(data)) > w.max {
		return &fs.PathError{Op: "write", Path: name, Err: ErrFileTooLarge}
	}
	op := FileCreated
	if info, err := w.inner.Stat(real); err == nil {
		if info.IsDir() {
			return &fs.PathError{Op: "write", Path: name, Err: fs.ErrExist}
		}
		op = FileChanged
	}
	if err := w.inner.WriteFile(real, data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	w.record(FileChangedData{Op: op, Path: real, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
	return nil
}

// Remove implements Files.
func (w *workspace) Remove(name string) error {
	real, err := w.check("remove", name, true)
	if err != nil {
		return err
	}
	if err := w.inner.Remove(real); err != nil {
		return err
	}
	w.record(FileChangedData{Op: FileRemoved, Path: real})
	return nil
}

// Rename implements Files.
func (w *workspace) Rename(oldname, newname string) error {
	oldReal, err := w.check("rename", oldname, true)
	if err != nil {
		return err
	}
	newReal, err := w.check("rename", newname, true)
	if err != nil {
		return err
	}
	if err := w.inner.Rename(oldReal, newReal); err != nil {
		return err
	}
	d := FileChangedData{Op: FileRenamed, Path: newReal, OldPath: oldReal}
	if info, err := w.inner.Stat(newReal); err == nil && info.Mode().IsRegular() {
		d.Size = info.Size()
		d.SHA256 = w.sum(newReal)
	}
	w.record(d)
	return nil
}

// sum returns the SHA-256 of a file in hex, or "" when the file is too
// large or cannot be read.
func (w *workspace) sum(real string) string {
	f, err := w.inner.Open(real)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if n, err := io.Copy(h, io.LimitReader(f, w.max+1)); err != nil || n > w.max {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (w *workspace) record(d FileChangedData) {
	if w.emit == nil {
		return
	}
	d.Path = w.full(d.Path)
	if d.OldPath != "" {
		d.OldPath = w.full(d.OldPath)
	}
	w.emit(d)
}

// matchAny reports whether a pattern of the list matches the real name or
// a directory above it.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if matchRule(p, name) {
			return true
		}
	}
	return false
}

// matchRule matches a pattern of tools.files.deny or readOnly, with the
// rules of .gitignore. A pattern without a slash matches one part of the
// name at any depth. A pattern with a slash matches from the root, and
// "**" matches any number of directories. A pattern that matches a
// directory matches every name under it.
func matchRule(pattern, name string) bool {
	pattern = strings.TrimSuffix(strings.TrimSpace(pattern), "/")
	if pattern == "" || name == "." {
		return false
	}
	parts := strings.Split(name, "/")
	if !strings.Contains(pattern, "/") {
		for _, part := range parts {
			if ok, _ := path.Match(pattern, part); ok {
				return true
			}
		}
		return false
	}
	return matchParts(strings.Split(strings.TrimPrefix(pattern, "/"), "/"), parts)
}

// matchParts reports whether the parts of a pattern match the first parts
// of a name.
func matchParts(pat, parts []string) bool {
	if len(pat) == 0 {
		return true
	}
	if pat[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchParts(pat[1:], parts[i:]) {
				return true
			}
		}
		return false
	}
	if len(parts) == 0 {
		return false
	}
	ok, _ := path.Match(pat[0], parts[0])
	return ok && matchParts(pat[1:], parts[1:])
}

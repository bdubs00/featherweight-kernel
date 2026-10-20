package fw

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

// dirOf returns the directory of the tools of rs, relative to the root of
// the files tool with slashes (section 7.9). It is "" for the root itself:
// always without tools.files.perRun, and for a run with FilesRoot. With
// tools.files.perRun, a run gets runs/<id>. A child run takes the
// directory of its parent, and a branch the directory of its source, so a
// tree of runs and its branches work in one directory, the directory of
// the run at the top. A source in another process names a directory of
// this process that starts empty.
func (a *Agent) dirOf(rs *runState) string {
	if !a.cfg.Tools.Files.PerRunOn() {
		return ""
	}
	rs.mu.Lock()
	d, ok := rs.dir, rs.dirSet
	rs.mu.Unlock()
	if ok {
		return d
	}
	d = a.findDir(rs)
	rs.mu.Lock()
	rs.dir, rs.dirSet = d, true
	rs.mu.Unlock()
	return d
}

// findDir walks up the parents and the sources of rs for dirOf. Only the
// kernel sets Parent and Kept, so a request that names fw.forkOf in its
// metadata does not reach the directory of another run.
func (a *Agent) findDir(rs *runState) string {
	cur, id := rs, rs.id
	for i := 0; i < 64 && cur != nil; i++ {
		q := cur.queuedData()
		next := ""
		switch {
		case cur.parent != "":
			next = cur.parent
		case q.Kept > 0 && q.Metadata[MetaForkOf] != "":
			next = q.Metadata[MetaForkOf]
		case q.FilesRoot:
			return ""
		default:
			return path.Join(RunsDir, cur.id)
		}
		if checkRunID(next) != nil {
			return path.Join(RunsDir, cur.id)
		}
		id = next
		cur, _ = a.k.lookup(next)
	}
	return path.Join(RunsDir, id)
}

// newRunDir returns the directory of a run that Start queues: the
// directory of its parent, the root for FilesRoot, or runs/<id>.
func (a *Agent) newRunDir(id string, parent *runState, filesRoot bool) string {
	switch {
	case !a.cfg.Tools.Files.PerRunOn():
		return ""
	case parent != nil:
		return a.dirOf(parent)
	case filesRoot:
		return ""
	}
	return path.Join(RunsDir, id)
}

// absDir returns the absolute path of a directory of dirOf, or "" when it
// cannot be named.
func (a *Agent) absDir(rel string) string {
	root, err := filepath.Abs(a.cfg.WorkspaceRoot())
	if err != nil {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// runWorkspace returns the workspace of the tools of a run in the
// directory rel, which records each change through emit. It creates the
// directory first (dirFiles.makeSub).
func (a *Agent) runWorkspace(rel string, emit func(FileChangedData)) (*workspace, error) {
	if rel == "" {
		return a.files.bind(emit), nil
	}
	if d, ok := a.files.inner.(*dirFiles); ok {
		if err := d.makeSub(rel, a.toolUser); err != nil {
			return nil, fmt.Errorf("fw: the directory %s of the run: %w", rel, err)
		}
	}
	return a.files.sub(rel).bind(emit), nil
}

// sub returns the workspace of the directory rel under w: its names are
// relative to rel, and the changes that it records carry the path under
// the root of w, so the files of a run read the same through Agent.Files.
// Its store opens rel itself as the root, so no name leaves rel, through
// a symbolic link either.
func (w *workspace) sub(rel string) *workspace {
	c := *w
	c.emit = nil
	if rel == "" {
		return &c
	}
	c.prefix = path.Join(w.prefix, rel)
	if d, ok := w.inner.(*dirFiles); ok {
		c.inner = &dirFiles{dir: d.dir, sub: path.Join(d.sub, rel), owner: d.owner}
	} else {
		c.inner = &prefixFiles{inner: w.inner, dir: rel}
	}
	c.reserved = nil
	for _, r := range w.reserved {
		switch {
		case r == "." || r == rel || strings.HasPrefix(rel, r+"/"):
			c.reserved = append(c.reserved, ".")
		case strings.HasPrefix(r, rel+"/"):
			c.reserved = append(c.reserved, r[len(rel)+1:])
		}
	}
	return &c
}

// full returns a name of w as a name under the root of the workspace of
// the agent.
func (w *workspace) full(name string) string {
	if w.prefix == "" {
		return name
	}
	return path.Join(w.prefix, name)
}

// shown returns a name under the root of the workspace of the agent as
// the tools of the run in dir see it: relative to dir when it lies there.
func shown(name, dir string) string {
	if dir != "" && strings.HasPrefix(name, dir+"/") {
		return name[len(dir)+1:]
	}
	return name
}

// prefixFiles is the directory dir of a store that is not a directory on
// the disk, such as a store in memory.
type prefixFiles struct {
	inner Files
	dir   string
}

var _ Files = (*prefixFiles)(nil)

func (p *prefixFiles) name(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return path.Join(p.dir, name), nil
}

// Open implements fs.FS.
func (p *prefixFiles) Open(name string) (fs.File, error) {
	n, err := p.name("open", name)
	if err != nil {
		return nil, err
	}
	return p.inner.Open(n)
}

// Stat implements fs.StatFS.
func (p *prefixFiles) Stat(name string) (fs.FileInfo, error) {
	n, err := p.name("stat", name)
	if err != nil {
		return nil, err
	}
	return p.inner.Stat(n)
}

// ReadDir implements fs.ReadDirFS.
func (p *prefixFiles) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := p.name("readdir", name)
	if err != nil {
		return nil, err
	}
	return p.inner.ReadDir(n)
}

// WriteFile implements Files.
func (p *prefixFiles) WriteFile(name string, data []byte) error {
	n, err := p.name("write", name)
	if err != nil {
		return err
	}
	return p.inner.WriteFile(n, data)
}

// Remove implements Files. It never removes the directory itself.
func (p *prefixFiles) Remove(name string) error {
	if name == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	n, err := p.name("remove", name)
	if err != nil {
		return err
	}
	return p.inner.Remove(n)
}

// Rename implements Files.
func (p *prefixFiles) Rename(oldname, newname string) error {
	if oldname == "." || newname == "." {
		return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrInvalid}
	}
	o, err := p.name("rename", oldname)
	if err != nil {
		return err
	}
	n, err := p.name("rename", newname)
	if err != nil {
		return err
	}
	return p.inner.Rename(o, n)
}

// RealName returns name as the inner store resolves it, relative to dir.
// A name that the inner store resolves outside dir is an error.
func (p *prefixFiles) RealName(name string) (string, error) {
	n, err := p.name("resolve", name)
	if err != nil {
		return "", err
	}
	real, err := RealName(p.inner, n)
	if err != nil {
		return "", err
	}
	if real == p.dir {
		return ".", nil
	}
	if !strings.HasPrefix(real, p.dir+"/") {
		return "", &fs.PathError{Op: "resolve", Path: name, Err: ErrFileOutside}
	}
	return real[len(p.dir)+1:], nil
}

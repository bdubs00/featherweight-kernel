package fw

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Files is the store that the file tools of an agent work in: the
// workspace (docs/ARCHITECTURE.md section 7.9). A name follows io/fs: it is
// slash-separated, relative, and clean, and "." is the root.
type Files interface {
	fs.StatFS
	fs.ReadDirFS
	// WriteFile replaces the file name with data and creates its parent
	// directories.
	WriteFile(name string, data []byte) error
	// Remove removes a file or a directory tree. It never removes ".".
	Remove(name string) error
	// Rename renames a file or a directory. It fails when newname exists.
	Rename(oldname, newname string) error
}

// Operations of FileChangedData.
const (
	FileCreated = "created"
	FileChanged = "changed"
	FileRemoved = "removed"
	FileRenamed = "renamed"
)

// Errors of the workspace. All but ErrFileTooLarge wrap fs.ErrPermission.
var (
	ErrFileOutside  = fmt.Errorf("the path leads outside the workspace: %w", fs.ErrPermission)
	ErrFileReserved = fmt.Errorf("the path holds the state or the configuration of fw: %w", fs.ErrPermission)
	ErrFileDenied   = fmt.Errorf("tools.files.deny names the path: %w", fs.ErrPermission)
	ErrFileReadOnly = fmt.Errorf("tools.files.readOnly names the path: %w", fs.ErrPermission)
	ErrFileTooLarge = errors.New("the file is larger than tools.files.maxFileBytes")
)

// DirFiles returns the files under dir. Each operation goes through
// os.Root, so no name leaves dir, through a symbolic link either. The
// directory need not exist until the first write. A new directory gets the
// permissions of dir, and a new file gets them without the execute bits,
// so a store under a private directory stays private.
func DirFiles(dir string) (Files, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("fw: files: %s: %w", dir, err)
	}
	return &dirFiles{dir: abs}, nil
}

// dirFiles is a directory on the local disk.
type dirFiles struct{ dir string }

// Dir returns the absolute directory.
func (d *dirFiles) Dir() string { return d.dir }

// root opens the directory and returns the local form of name.
func (d *dirFiles) root(op, name string) (*os.Root, string, error) {
	local, err := localName(op, name)
	if err != nil {
		return nil, "", err
	}
	r, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, "", &fs.PathError{Op: op, Path: name, Err: errors.Unwrap(err)}
	}
	return r, local, nil
}

// localName checks a name and returns it with the separator of the system.
func localName(op, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return filepath.FromSlash(name), nil
}

// Open implements fs.FS.
func (d *dirFiles) Open(name string) (fs.File, error) {
	r, local, err := d.root("open", name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Open(local)
}

// Stat implements fs.StatFS.
func (d *dirFiles) Stat(name string) (fs.FileInfo, error) {
	r, local, err := d.root("stat", name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return r.Stat(local)
}

// ReadDir implements fs.ReadDirFS. The entries are sorted by name.
func (d *dirFiles) ReadDir(name string) ([]fs.DirEntry, error) {
	r, local, err := d.root("readdir", name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(local)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// WriteFile implements Files. An existing file keeps its mode.
func (d *dirFiles) WriteFile(name string, data []byte) error {
	if err := os.MkdirAll(d.dir, 0o755); err != nil {
		return &fs.PathError{Op: "write", Path: name, Err: errors.Unwrap(err)}
	}
	r, local, err := d.root("write", name)
	if err != nil {
		return err
	}
	defer r.Close()
	mode := d.mode()
	if dir := filepath.Dir(local); dir != "." {
		if err := r.MkdirAll(dir, mode); err != nil {
			return err
		}
	}
	return r.WriteFile(local, data, mode&^0o111)
}

// mode returns the permissions of the directory, or 0755.
func (d *dirFiles) mode() fs.FileMode {
	if info, err := os.Stat(d.dir); err == nil {
		return info.Mode().Perm()
	}
	return 0o755
}

// Remove implements Files.
func (d *dirFiles) Remove(name string) error {
	if name == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	r, local, err := d.root("remove", name)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Lstat(local); err != nil {
		return err
	}
	return r.RemoveAll(local)
}

// Rename implements Files.
func (d *dirFiles) Rename(oldname, newname string) error {
	if oldname == "." || newname == "." {
		return &fs.PathError{Op: "rename", Path: oldname, Err: fs.ErrInvalid}
	}
	newLocal, err := localName("rename", newname)
	if err != nil {
		return err
	}
	r, oldLocal, err := d.root("rename", oldname)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Lstat(oldLocal); err != nil {
		return err
	}
	if _, err := r.Lstat(newLocal); err == nil {
		return &fs.PathError{Op: "rename", Path: newname, Err: fs.ErrExist}
	}
	if dir := filepath.Dir(newLocal); dir != "." {
		if err := r.MkdirAll(dir, d.mode()); err != nil {
			return err
		}
	}
	return r.Rename(oldLocal, newLocal)
}

// RealName returns name after its symbolic links, relative to the
// directory. A name that leads outside the directory is an error. A part of
// the name that does not exist yet stays as it is.
func (d *dirFiles) RealName(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "resolve", Path: name, Err: fs.ErrInvalid}
	}
	real := evalExisting(d.dir)
	rel, err := filepath.Rel(real, evalExisting(filepath.Join(real, filepath.FromSlash(name))))
	if err != nil || !filepath.IsLocal(rel) {
		return "", &fs.PathError{Op: "resolve", Path: name, Err: ErrFileOutside}
	}
	return filepath.ToSlash(rel), nil
}

// evalExisting resolves the symbolic links of the longest existing prefix
// of p and appends the rest unchanged.
func evalExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// RealName returns name as the store resolves it: after symbolic links
// for DirFiles, and clean for a store that does not resolve names.
func RealName(f Files, name string) (string, error) {
	if r, ok := f.(interface{ RealName(string) (string, error) }); ok {
		return r.RealName(name)
	}
	if !fs.ValidPath(name) {
		return "", &fs.PathError{Op: "resolve", Path: name, Err: fs.ErrInvalid}
	}
	return path.Clean(name), nil
}

// FileChangedData is the payload of fw.file.changed: one change that a
// tool call made in the workspace. It never holds the content.
type FileChangedData struct {
	CallID  string `json:"callId,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Op      string `json:"op"`
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256,omitempty"`
}

// RunFile is one file that a run changed, with its last change. Op is
// created when the run created the file and then changed it.
type RunFile struct {
	Path   string    `json:"path"`
	Op     string    `json:"op"`
	Size   int64     `json:"size"`
	SHA256 string    `json:"sha256,omitempty"`
	Tool   string    `json:"tool,omitempty"`
	CallID string    `json:"callId,omitempty"`
	Seq    uint64    `json:"seq"`
	Time   time.Time `json:"time"`
}

// RunFiles reads the events of a run and returns one entry for each path
// that the run changed, with its last change, in the order of the first
// change. A file that the run created and then changed stays created. The
// old path of a rename becomes an entry with the operation removed.
func RunFiles(events []Event) []RunFile {
	var out []RunFile
	at := map[string]int{}
	put := func(f RunFile) {
		if i, ok := at[f.Path]; ok {
			if out[i].Op == FileCreated && f.Op == FileChanged {
				f.Op = FileCreated
			}
			out[i] = f
			return
		}
		at[f.Path] = len(out)
		out = append(out, f)
	}
	for _, e := range events {
		if e.Type != EvFileChanged {
			continue
		}
		var d FileChangedData
		if e.Decode(&d) != nil || d.Path == "" {
			continue
		}
		f := RunFile{Path: d.Path, Op: d.Op, Size: d.Size, SHA256: d.SHA256, Tool: d.Tool, CallID: d.CallID, Seq: e.Seq, Time: e.Time}
		if d.Op == FileRenamed && d.OldPath != "" {
			put(RunFile{Path: d.OldPath, Op: FileRemoved, Tool: d.Tool, CallID: d.CallID, Seq: e.Seq, Time: e.Time})
		}
		put(f)
	}
	return out
}

type filesKey struct{}

// withFiles stores the workspace of a tool call in the context.
func withFiles(ctx context.Context, f Files) context.Context {
	return context.WithValue(ctx, filesKey{}, f)
}

// FilesFrom returns the workspace of a tool call: the workspace of the
// agent, which records each change in the ledger of the run. Outside a run,
// it is the workspace of the agent in the context, which records nothing.
// It is nil when the context holds neither.
func FilesFrom(ctx context.Context) Files {
	if f, ok := ctx.Value(filesKey{}).(Files); ok {
		return f
	}
	if a := AgentFrom(ctx); a != nil {
		return a.Files()
	}
	return nil
}

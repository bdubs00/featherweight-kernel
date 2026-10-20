package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("files", newFiles) }

const filesSchema = `{
  "type": "object",
  "description": "The input of the Anthropic text editor tool (text_editor_20250728).",
  "properties": {
    "command": {
      "type": "string",
      "enum": ["view", "create", "str_replace", "insert"],
      "description": "view shows a file with line numbers, or lists a directory two levels deep. create writes a file. str_replace replaces text that appears exactly once. insert adds text after a line."
    },
    "path": {
      "type": "string",
      "description": "The file or directory. Give an absolute path under the root, or a path relative to the root."
    },
    "file_text": {
      "type": "string",
      "description": "For create: the full content of the file."
    },
    "old_str": {
      "type": "string",
      "description": "For str_replace: the exact text to replace, with its whitespace. It must appear exactly once in the file."
    },
    "new_str": {
      "type": "string",
      "description": "For str_replace: the new text. Leave it out to delete old_str. For insert, it is used when insert_text is absent."
    },
    "insert_line": {
      "type": "integer",
      "minimum": 0,
      "description": "For insert: the line after which the text goes. 0 puts the text at the top of the file."
    },
    "insert_text": {
      "type": "string",
      "description": "For insert: the text to insert."
    },
    "view_range": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 2,
      "maxItems": 2,
      "description": "For view of a file: [start, end], the first and last line to show, counting from 1. An end of -1 means the end of the file."
    }
  },
  "required": ["command", "path"],
  "additionalProperties": false
}`

const filesDescription = "View, create, and edit text files under the workspace root. " +
	"view shows a file with line numbers, or lists a directory two levels deep. " +
	"create writes a whole file and makes missing parent directories. " +
	"str_replace replaces text that appears exactly once. " +
	"insert adds text after a line number, and line 0 means the top. " +
	"Every path stays inside the root."

// filesCommands are the commands of the files tool.
var filesCommands = []string{"view", "create", "str_replace", "insert"}

// filesTool is the text editor on the workspace (section 7.9). The kernel
// gives each call the workspace of the agent through fw.FilesFrom, which
// checks every name and records every change. own serves a call outside
// an agent, such as a test.
type filesTool struct {
	ed   *editor
	own  fw.Files
	root string // the absolute root of own
}

var _ fw.Resolver = (*filesTool)(nil)

func newFiles(cfg *fw.Config) (fw.Tool, error) {
	if cfg == nil {
		cfg = fw.DefaultConfig()
	}
	root := cfg.WorkspaceRoot()
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("builtin: files: root %s: %w", root, err)
	}
	dir, err := fw.DirFiles(abs)
	if err != nil {
		return nil, fmt.Errorf("builtin: files: %w", err)
	}
	own, err := fw.NewWorkspace(dir, cfg)
	if err != nil {
		return nil, fmt.Errorf("builtin: files: %w", err)
	}
	return &filesTool{own: own, root: abs, ed: filesEditor(abs)}, nil
}

// filesEditor returns the editor of the files tool on the directory dir.
func filesEditor(dir string) *editor {
	return &editor{
		tool:     "files",
		commands: filesCommands,
		name:     rootName(dir),
		show:     func(name string) string { return filepath.Join(dir, filepath.FromSlash(name)) },
	}
}

// editor returns the editor of a call: on the directory of the run with
// tools.files.perRun, which the kernel names in the run info next to the
// workspace of that directory, and on the root otherwise.
func (t *filesTool) editor(ctx context.Context) *editor {
	if dir := fw.RunInfoFrom(ctx).Dir; dir != "" && dir != t.root && fw.FilesFrom(ctx) != nil {
		return filesEditor(dir)
	}
	return t.ed
}

// Spec implements fw.Tool.
func (t *filesTool) Spec() fw.ToolSpec {
	s := newSpec("files", filesDescription, filesSchema)
	s.Native = fw.NativeTextEditor
	return s
}

// files returns the workspace of a call.
func (t *filesTool) files(ctx context.Context) fw.Files {
	if f := fw.FilesFrom(ctx); f != nil {
		return f
	}
	return t.own
}

// Call implements fw.Tool.
func (t *filesTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	return t.editor(ctx).call(ctx, t.files(ctx), input)
}

// Resolve implements fw.Resolver. It names the file of a call by its real
// path relative to the root, with slashes. A policy rule such as
// "files: path=secrets/*" then also matches "./secrets/a", an absolute path
// under the root, and a symbolic link into secrets.
func (t *filesTool) Resolve(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var in map[string]json.RawMessage
	if err := decode(input, &in); err != nil {
		return nil, err
	}
	raw, ok := in["path"]
	if !ok {
		return input, nil
	}
	var p string
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	name, err := t.editor(ctx).name(p)
	if err != nil {
		return nil, err
	}
	real, err := fw.RealName(t.files(ctx), name)
	if err != nil {
		return nil, err
	}
	in["path"], _ = json.Marshal(real)
	return json.Marshal(in)
}

// rootName maps a model path to a name under root. It accepts a path
// relative to root and an absolute path under root or under the root after
// its symbolic links.
func rootName(root string) func(p string) (string, error) {
	return func(p string) (string, error) {
		if p == "" {
			return "", errors.New("the path is empty")
		}
		full := p
		if !filepath.IsAbs(full) {
			full = filepath.Join(root, full)
		}
		full = filepath.Clean(full)
		for _, base := range []string{root, evalLinks(root)} {
			if rel, err := filepath.Rel(base, full); err == nil && filepath.IsLocal(rel) {
				return filepath.ToSlash(rel), nil
			}
		}
		return "", fmt.Errorf("the path %s is outside the root %s", p, root)
	}
}

// evalLinks returns p after its symbolic links, or p when it does not exist.
func evalLinks(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// editor runs the text editor commands on a store. The files tool and the
// memory tool share it.
type editor struct {
	tool     string
	commands []string
	// name maps a model path to a name of the store, or fails with a
	// message for the model.
	name func(p string) (string, error)
	// show maps a name of the store to the path that the model sees.
	show func(name string) string
}

// editInput is the input of the files tool and the memory tool.
type editInput struct {
	Command    string  `json:"command"`
	Path       string  `json:"path"`
	FileText   *string `json:"file_text"`
	OldStr     *string `json:"old_str"`
	NewStr     *string `json:"new_str"`
	InsertLine *int    `json:"insert_line"`
	InsertText *string `json:"insert_text"`
	ViewRange  []int   `json:"view_range"`
	OldPath    string  `json:"old_path"`
	NewPath    string  `json:"new_path"`
}

func (e *editor) call(ctx context.Context, files fw.Files, input json.RawMessage) (fw.ToolResult, error) {
	var in editInput
	if err := decode(input, &in); err != nil {
		return badInput(e.tool, err), nil
	}
	if err := ctx.Err(); err != nil {
		return canceled(ctx, e.tool)
	}
	if !slices.Contains(e.commands, in.Command) {
		return fw.ErrorResult(fmt.Sprintf("Error: unknown command %q. Use one of: %s.", in.Command, strings.Join(e.commands, ", "))), nil
	}
	text, err := e.run(files, in)
	if err != nil {
		return fw.ErrorResult(err.Error()), nil
	}
	return fw.TextResult(text), nil
}

func (e *editor) run(files fw.Files, in editInput) (string, error) {
	if in.Command == "rename" {
		return e.rename(files, in.OldPath, in.NewPath)
	}
	name, err := e.name(in.Path)
	if err != nil {
		return "", fmt.Errorf("Error: %v", err)
	}
	shown := e.show(name)
	switch in.Command {
	case "view":
		return e.view(files, name, shown, in.ViewRange)
	case "create":
		if in.FileText == nil {
			return "", errors.New("Error: file_text is required for the create command.")
		}
		return e.create(files, name, shown, *in.FileText)
	case "str_replace":
		if in.OldStr == nil || *in.OldStr == "" {
			return "", errors.New("Error: old_str is required for the str_replace command and must not be empty.")
		}
		return e.strReplace(files, name, shown, *in.OldStr, deref(in.NewStr))
	case "insert":
		if in.InsertLine == nil {
			return "", errors.New("Error: insert_line is required for the insert command.")
		}
		text := in.InsertText
		if text == nil {
			text = in.NewStr
		}
		if text == nil {
			return "", errors.New("Error: insert_text is required for the insert command.")
		}
		return e.insert(files, name, shown, *in.InsertLine, *text)
	case "delete":
		return e.delete(files, name, shown)
	}
	return "", fmt.Errorf("Error: unknown command %q.", in.Command)
}

// refused explains an error of the workspace rules to the model, or
// returns nil for any other error.
func (e *editor) refused(shown string, err error) error {
	switch {
	case errors.Is(err, fw.ErrFileReserved):
		return fmt.Errorf("Error: %s holds the state or the configuration of fw, and the %s tool does not open it.", shown, e.tool)
	case errors.Is(err, fw.ErrFileDenied):
		return fmt.Errorf("Error: %s is closed to the %s tool (tools.files.deny).", shown, e.tool)
	case errors.Is(err, fw.ErrFileReadOnly):
		return fmt.Errorf("Error: %s is read-only for the %s tool (tools.files.readOnly).", shown, e.tool)
	case errors.Is(err, fw.ErrFileTooLarge):
		return fmt.Errorf("Error: %s is larger than the limit of the %s tool (tools.files.maxFileBytes).", shown, e.tool)
	case errors.Is(err, fw.ErrFileOutside):
		return fmt.Errorf("Error: the path %s leads outside %s through a symbolic link.", shown, e.show("."))
	}
	return nil
}

// fail returns the message for err: a refusal of the rules, or the
// message that msg gives.
func (e *editor) fail(shown string, err error, msg func(error) error) error {
	if r := e.refused(shown, err); r != nil {
		return r
	}
	return msg(cause(err))
}

func (e *editor) view(files fw.Files, name, shown string, rng []int) (string, error) {
	info, err := files.Stat(name)
	if err != nil {
		return "", e.fail(shown, err, func(error) error {
			return fmt.Errorf("The path %s does not exist. Please provide a valid path.", shown)
		})
	}
	if info.IsDir() {
		if len(rng) > 0 {
			return "", errors.New("Error: view_range applies to files only, not to directories.")
		}
		return e.list(files, name)
	}
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return "", e.fail(shown, err, func(c error) error { return fmt.Errorf("Error: %s cannot be read: %v", shown, c) })
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return "", fmt.Errorf("Error: %s is a binary file. view shows text files only.", shown)
	}
	lines := splitLines(string(data))
	start, end := 1, len(lines)
	if len(rng) > 0 {
		if len(rng) != 2 {
			return "", errors.New("Error: view_range needs two numbers, [start, end].")
		}
		start, end = rng[0], rng[1]
		if start < 1 || start > len(lines) {
			return "", fmt.Errorf("Error: Invalid view_range %v. The start must be in [1, %d].", rng, len(lines))
		}
		if end == -1 || end > len(lines) {
			end = len(lines)
		}
		if end < start {
			return "", fmt.Errorf("Error: Invalid view_range %v. The end must be -1 or not less than the start.", rng)
		}
	}
	out := fmt.Sprintf("Here's the content of %s with line numbers:\n%s", shown, numbered(lines[start-1:end], start))
	out, cut := truncate(out, maxOutput)
	if cut {
		out += "\n[output truncated at 100 KB; use view_range to see the rest]"
	}
	return out, nil
}

// list shows a directory two levels deep, without hidden items and
// node_modules, with a size for each entry. The workspace leaves out what
// its rules refuse.
func (e *editor) list(files fw.Files, name string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Here're the files and directories up to 2 levels deep in %s, excluding hidden items and node_modules:\n", e.show(name))
	_ = fs.WalkDir(files, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && p != name {
				return fs.SkipDir
			}
			return nil
		}
		depth := 0
		if p != name {
			sub := p
			if name != "." {
				sub = strings.TrimPrefix(p, name+"/")
			}
			depth = strings.Count(sub, "/") + 1
			if base := d.Name(); strings.HasPrefix(base, ".") || base == "node_modules" {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		size := int64(0)
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}
		fmt.Fprintf(&b, "%s\t%s\n", humanSize(size), e.show(p))
		if d.IsDir() && depth >= 2 {
			return fs.SkipDir
		}
		return nil
	})
	out, cut := truncate(strings.TrimRight(b.String(), "\n"), maxOutput)
	if cut {
		out += "\n[listing truncated at 100 KB]"
	}
	return out, nil
}

func (e *editor) create(files fw.Files, name, shown, text string) (string, error) {
	if info, err := files.Stat(name); err == nil && info.IsDir() {
		return "", fmt.Errorf("Error: %s is a directory. create writes files only.", shown)
	}
	if err := files.WriteFile(name, []byte(text)); err != nil {
		return "", e.fail(shown, err, func(c error) error { return fmt.Errorf("Error: %s cannot be written: %v", shown, c) })
	}
	return "File created successfully at: " + shown, nil
}

// readText reads a regular file for an edit.
func (e *editor) readText(files fw.Files, name, shown string) (string, error) {
	info, err := files.Stat(name)
	if err != nil || info.IsDir() {
		if r := e.refused(shown, err); r != nil {
			return "", r
		}
		return "", fmt.Errorf("Error: The path %s does not exist. Please provide a valid path.", shown)
	}
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return "", e.fail(shown, err, func(c error) error { return fmt.Errorf("Error: %s cannot be read: %v", shown, c) })
	}
	return string(data), nil
}

// write replaces a file for an edit.
func (e *editor) write(files fw.Files, name, shown, content string) error {
	if err := files.WriteFile(name, []byte(content)); err != nil {
		return e.fail(shown, err, func(c error) error { return fmt.Errorf("Error: %s cannot be written: %v", shown, c) })
	}
	return nil
}

func (e *editor) strReplace(files fw.Files, name, shown, old, repl string) (string, error) {
	content, err := e.readText(files, name, shown)
	if err != nil {
		return "", err
	}
	var at []int
	for i := 0; ; {
		j := strings.Index(content[i:], old)
		if j < 0 {
			break
		}
		at = append(at, i+j)
		i += j + len(old)
	}
	switch len(at) {
	case 0:
		return "", fmt.Errorf("No replacement was performed, old_str `%s` did not appear verbatim in %s.", old, shown)
	case 1:
	default:
		lines := make([]string, len(at))
		for k, i := range at {
			lines[k] = strconv.Itoa(strings.Count(content[:i], "\n") + 1)
		}
		return "", fmt.Errorf("No replacement was performed. Multiple occurrences of old_str `%s` in lines: [%s]. Please ensure it is unique", old, strings.Join(lines, ", "))
	}
	updated := content[:at[0]] + repl + content[at[0]+len(old):]
	if err := e.write(files, name, shown, updated); err != nil {
		return "", err
	}
	first := strings.Count(content[:at[0]], "\n")
	return edited(shown, updated, first, first+strings.Count(repl, "\n")), nil
}

func (e *editor) insert(files fw.Files, name, shown string, line int, text string) (string, error) {
	content, err := e.readText(files, name, shown)
	if err != nil {
		return "", err
	}
	var lines []string
	if content != "" {
		lines = splitLines(content)
	}
	if line < 0 || line > len(lines) {
		return "", fmt.Errorf("Error: Invalid `insert_line` parameter: %d. It should be within the range of lines of the file: [0, %d]", line, len(lines))
	}
	added := splitLines(text)
	updated := strings.Join(slices.Concat(lines[:line], added, lines[line:]), "\n")
	if strings.HasSuffix(content, "\n") || (content == "" && strings.HasSuffix(text, "\n")) {
		updated += "\n"
	}
	if err := e.write(files, name, shown, updated); err != nil {
		return "", err
	}
	return edited(shown, updated, line, line+len(added)-1), nil
}

func (e *editor) delete(files fw.Files, name, shown string) (string, error) {
	if name == "." {
		return "", fmt.Errorf("Error: %s itself cannot be deleted.", e.show("."))
	}
	if err := files.Remove(name); err != nil {
		return "", e.fail(shown, err, func(c error) error {
			if errors.Is(c, fs.ErrNotExist) {
				return fmt.Errorf("Error: The path %s does not exist", shown)
			}
			return fmt.Errorf("Error: %s cannot be deleted: %v", shown, c)
		})
	}
	return "Successfully deleted " + shown, nil
}

func (e *editor) rename(files fw.Files, oldPath, newPath string) (string, error) {
	oldName, err := e.name(oldPath)
	if err != nil {
		return "", fmt.Errorf("Error: %v", err)
	}
	newName, err := e.name(newPath)
	if err != nil {
		return "", fmt.Errorf("Error: %v", err)
	}
	oldShown, newShown := e.show(oldName), e.show(newName)
	if oldName == "." {
		return "", fmt.Errorf("Error: %s itself cannot be renamed.", e.show("."))
	}
	if err := files.Rename(oldName, newName); err != nil {
		return "", e.fail(oldShown, err, func(c error) error {
			switch {
			case errors.Is(c, fs.ErrExist):
				return fmt.Errorf("Error: The destination %s already exists", newShown)
			case errors.Is(c, fs.ErrNotExist):
				return fmt.Errorf("Error: The path %s does not exist", oldShown)
			}
			return fmt.Errorf("Error: %s cannot be renamed: %v", oldShown, c)
		})
	}
	return fmt.Sprintf("Successfully renamed %s to %s", oldShown, newShown), nil
}

// edited reports an edit with the changed lines (0-based, inclusive) and
// four lines of context on each side.
func edited(shown, content string, first, last int) string {
	lines := splitLines(content)
	start := max(0, first-4)
	end := min(len(lines), last+5)
	if start >= end {
		return fmt.Sprintf("The file %s has been edited.", shown)
	}
	return fmt.Sprintf("The file %s has been edited. Here are the changed lines with line numbers:\n%s\nReview the changes, and edit the file again if they are wrong.",
		shown, numbered(lines[start:end], start+1))
}

// splitLines splits text into lines. A final newline ends the last line and
// does not start a new one.
func splitLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// numbered formats lines like cat -n, starting at line number first.
func numbered(lines []string, first int) string {
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%6d\t%s", first+i, l)
	}
	return b.String()
}

// humanSize formats a byte count like du -h.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f, suffix := float64(n)/unit, "K"
	for _, s := range []string{"M", "G", "T"} {
		if f < unit {
			break
		}
		f, suffix = f/unit, s
	}
	return fmt.Sprintf("%.1f%s", f, suffix)
}

// cause returns the reason inside a path error, so a message names the path
// that the model gave and not the host path.
func cause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

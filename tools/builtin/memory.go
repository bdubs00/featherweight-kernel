package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

func init() { fw.RegisterTool("memory", newMemory) }

const memorySchema = `{
  "type": "object",
  "description": "The input of the Anthropic memory tool (memory_20250818). Every path starts with /memories.",
  "properties": {
    "command": {
      "type": "string",
      "enum": ["view", "create", "str_replace", "insert", "delete", "rename"],
      "description": "view shows a file with line numbers, or lists a directory two levels deep. create writes a file. str_replace replaces text that appears exactly once. insert adds text after a line. delete removes a file or a directory. rename moves a file or a directory."
    },
    "path": {
      "type": "string",
      "description": "The file or directory, for example /memories/notes.md. Every command but rename needs it."
    },
    "file_text": {
      "type": "string",
      "description": "For create: the full content of the file."
    },
    "old_str": {
      "type": "string",
      "description": "For str_replace: the exact text to replace. It must appear exactly once in the file."
    },
    "new_str": {
      "type": "string",
      "description": "For str_replace: the new text. Leave it out to delete old_str."
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
    },
    "old_path": {
      "type": "string",
      "description": "For rename: the current path."
    },
    "new_path": {
      "type": "string",
      "description": "For rename: the new path. It must not exist."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`

const memoryDescription = "Keep notes that last between runs, in files under /memories. " +
	"The system prompt lists each file with its first line. " +
	"view shows a file with line numbers, or lists a directory. " +
	"create, str_replace, and insert write files. delete and rename change files and directories. " +
	"Never store secrets, keys, or passwords in memory."

// memoryCommands are the commands of the memory tool.
var memoryCommands = []string{"view", "create", "str_replace", "insert", "delete", "rename"}

// memoryPrefix is the path that the model uses for the memory directory.
const memoryPrefix = "/memories"

// Bounds of the memory index in the system prompt.
const (
	briefMaxFiles = 40      // files in the list
	briefMaxBytes = 4 << 10 // bytes of the list lines
	briefReadMax  = 2 << 10 // bytes read from each file for its first line
	briefLineMax  = 160     // characters of one first line
)

// Texts of the memory section of the system prompt.
const (
	briefHeading = "# Memory\n\n"
	briefIntro   = "The memory tool keeps notes between runs under /memories. "
	briefList    = "Each line below names a file and its first line, newest first. View a file when its line matters for the task."
	briefEmpty   = "The memory holds no notes yet."
	briefRules   = "Save what a later run needs and cannot find in the workspace: a preference of the user, " +
		"a decision and its reason, a fact that took effort to find. Start each file with one line that says what it holds, " +
		"because that line appears here at the start of every run. Keep one topic in each file. " +
		"Update a file instead of adding a second one, and delete a note that turns out to be wrong. " +
		"Never store a secret, a key, or a password."
)

// memoryTool is the text editor on a memory directory under <home>/memory,
// addressed as /memories. With the agent scope, each agent profile has the
// directory agents/<agent>. With the shared scope, every profile uses the
// directory shared.
type memoryTool struct {
	base  string // <home>/memory, absolute
	scope string // fw.MemoryScopeAgent or fw.MemoryScopeShared
	ed    editor // the editor of every call, without its root
}

var _ fw.Briefer = (*memoryTool)(nil)

func newMemory(cfg *fw.Config) (fw.Tool, error) {
	home, scope := "", ""
	if cfg != nil {
		home, scope = cfg.Home, cfg.Tools.Memory.Scope
	}
	if home == "" {
		home = fw.DefaultHome()
	}
	switch scope = strings.ToLower(strings.TrimSpace(scope)); scope {
	case "", fw.MemoryScopeAgent:
		scope = fw.MemoryScopeAgent
	case fw.MemoryScopeShared:
	default:
		return nil, fmt.Errorf("builtin: memory: tools.memory.scope is %q, want %s or %s", scope, fw.MemoryScopeAgent, fw.MemoryScopeShared)
	}
	base, err := filepath.Abs(filepath.Join(home, "memory"))
	if err != nil {
		return nil, fmt.Errorf("builtin: memory: home %s: %w", home, err)
	}
	return &memoryTool{base: base, scope: scope, ed: editor{
		tool:     "memory",
		commands: memoryCommands,
		name:     memoryName,
		show:     memoryShow,
	}}, nil
}

// Spec implements fw.Tool.
func (t *memoryTool) Spec() fw.ToolSpec {
	s := newSpec("memory", memoryDescription, memorySchema)
	s.Native = fw.NativeMemory
	s.Idempotent = true
	return s
}

// Call implements fw.Tool. The run in the context selects the directory.
func (t *memoryTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	dir, err := t.dir(fw.RunInfoFrom(ctx).Agent)
	if err != nil {
		return fw.ErrorResult("Error: " + err.Error()), nil
	}
	// The directory is private, and the files in it take its mode.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fw.ErrorResult(fmt.Sprintf("Error: %s cannot be created: %v", memoryPrefix, cause(err))), nil
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return fw.ErrorResult("Error: " + err.Error()), nil
	}
	return t.ed.call(ctx, files, input)
}

// dir returns the memory directory of an agent. An empty agent is the
// profile "default".
func (t *memoryTool) dir(agent string) (string, error) {
	if t.scope == fw.MemoryScopeShared {
		return filepath.Join(t.base, "shared"), nil
	}
	if agent == "" {
		agent = "default"
	}
	if !validAgentName(agent) {
		return "", fmt.Errorf("the agent name %q cannot name a memory directory", agent)
	}
	return filepath.Join(t.base, "agents", agent), nil
}

// validAgentName applies the rule of profile names: letters, digits, '-',
// '_', and '.', at most 64 bytes, and no leading '.'. Such a name is one
// path element inside the memory directory.
func validAgentName(name string) bool {
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

// Brief implements fw.Briefer. It returns the memory section of the system
// prompt: every note as its path and its first line, newest first, and the
// rules for new notes. It creates no directory.
func (t *memoryTool) Brief(ctx context.Context, run fw.RunInfo) (string, error) {
	dir, err := t.dir(run.Agent)
	if err != nil {
		return "", fmt.Errorf("builtin: memory: %w", err)
	}
	lines, more, err := memoryIndex(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("builtin: memory: index of %s: %w", dir, err)
	}
	var sb strings.Builder
	sb.WriteString(briefHeading)
	sb.WriteString(briefIntro)
	if len(lines) == 0 {
		sb.WriteString(briefEmpty)
	} else {
		sb.WriteString(briefList)
		sb.WriteString("\n\n")
		sb.WriteString(strings.Join(lines, "\n"))
		if more > 0 {
			fmt.Fprintf(&sb, "\n%d more files are not listed. View %s to see every file.", more, memoryPrefix)
		}
	}
	sb.WriteString("\n\n")
	sb.WriteString(briefRules)
	return sb.String(), nil
}

// memoryNote is one file of the memory directory.
type memoryNote struct {
	path string // slash-separated, relative to the directory
	mod  time.Time
}

// memoryIndex lists the notes of dir as "- /memories/<path>: <first line>",
// newest first, within briefMaxFiles and briefMaxBytes. more counts the
// notes that do not fit. A missing directory has no notes. Every read goes
// through os.Root, so a symbolic link cannot leave the directory.
func memoryIndex(ctx context.Context, dir string) (lines []string, more int, err error) {
	r, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	fsys := r.FS()
	var notes []memoryNote
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil // an unreadable entry leaves the list
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		notes = append(notes, memoryNote{path: p, mod: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	slices.SortFunc(notes, func(a, b memoryNote) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.path, b.path)
	})
	size := 0
	for _, n := range notes {
		line := "- " + memoryPrefix + "/" + n.path
		if hook := firstLine(fsys, n.path); hook != "" {
			line += ": " + hook
		}
		if len(lines) == briefMaxFiles || (len(lines) > 0 && size+len(line)+1 > briefMaxBytes) {
			break
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	return lines, len(notes) - len(lines), nil
}

// firstLine returns the line that says what a note holds: the description
// of a front matter block, or else the first line with text, without its
// leading '#' marks. It reads at most briefReadMax bytes and cuts the line
// to briefLineMax characters. A file that cannot be read has no line.
func firstLine(fsys fs.FS, name string) string {
	f, err := fsys.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, briefReadMax))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ToValidUTF8(string(buf), ""), "\n")
	i := 0
	if strings.TrimSpace(lines[0]) == "---" {
		for i = 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if l == "---" {
				i++
				break
			}
			if v, ok := strings.CutPrefix(l, "description:"); ok {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					return clipLine(v)
				}
			}
		}
	}
	for ; i < len(lines); i++ {
		if l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "#")); l != "" {
			return clipLine(l)
		}
	}
	return ""
}

// clipLine replaces control characters with spaces and cuts s to
// briefLineMax characters, with "..." after a cut.
func clipLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= briefLineMax {
		return s
	}
	n := 0
	for i := range s {
		if n == briefLineMax {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

// memoryName maps /memories/<rest> to <rest>, relative to the memory
// directory, with slashes.
func memoryName(p string) (string, error) {
	rest, ok := strings.CutPrefix(p, memoryPrefix)
	if !ok || (rest != "" && rest[0] != '/') {
		return "", fmt.Errorf("the path %q is outside %s. Every memory path starts with %s", p, memoryPrefix, memoryPrefix)
	}
	rest = strings.TrimLeft(rest, "/")
	if rest == "" {
		return ".", nil
	}
	if !filepath.IsLocal(rest) {
		return "", fmt.Errorf("the path %q leaves %s", p, memoryPrefix)
	}
	return filepath.ToSlash(filepath.Clean(rest)), nil
}

// memoryShow maps a name of the memory directory to /memories/...
func memoryShow(name string) string {
	if name == "." || name == "" {
		return memoryPrefix
	}
	return memoryPrefix + "/" + name
}

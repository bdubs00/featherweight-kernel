package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
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
	"The first message of a run lists each file with its first line. " +
	"view shows a file with line numbers, or lists a directory. " +
	"create, str_replace, and insert write files. delete and rename change files and directories. " +
	"Never store secrets, keys, or passwords in memory."

// memoryCommands are the commands of the memory tool.
var memoryCommands = []string{"view", "create", "str_replace", "insert", "delete", "rename"}

// memoryPrefix is the path that the model uses for the memory directory.
const memoryPrefix = memory.Prefix

// memoryTool is the text editor on the notes of the scope of the run,
// addressed as /memories. The store of package memory selects the
// directory, keeps the limits, and records each change.
type memoryTool struct {
	store *memory.Store
	ed    editor // the editor of every call, without its root
}

var (
	_ fw.Briefer         = (*memoryTool)(nil)
	_ fw.ContextBriefer  = (*memoryTool)(nil)
	_ fw.CallIdempotency = (*memoryTool)(nil)
)

func newMemory(cfg *fw.Config) (fw.Tool, error) {
	store, err := memory.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("builtin: %w", err)
	}
	return &memoryTool{store: store, ed: editor{
		tool:     "memory",
		commands: memoryCommands,
		name:     memoryName,
		show:     memoryShow,
	}}, nil
}

// Close implements io.Closer.
func (t *memoryTool) Close() error { return t.store.Close() }

// Spec implements fw.Tool. The spec leaves Idempotent false, so a policy
// rule "idempotent" does not match a tool that writes notes; the tool
// tells the kernel for each call through IdempotentCall that the call is
// safe to repeat.
func (t *memoryTool) Spec() fw.ToolSpec {
	s := newSpec("memory", memoryDescription, memorySchema)
	s.Native = fw.NativeMemory
	return s
}

// memoryCommand returns the command of an input, or "" when the input
// cannot be read.
func memoryCommand(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.Command
}

// IdempotentCall implements fw.CallIdempotency. Every call is safe to
// repeat after a restart cut it before its result, so an unattended run
// never waits for a person on a note. A second view, create, delete, or
// rename leaves the notes as the first one did, or fails because the first
// one already moved or removed the note. insert and str_replace record the
// call before they write (editOnce), and a repeat that finds the note as
// the first call left it returns the first result.
func (t *memoryTool) IdempotentCall(json.RawMessage) bool { return true }

// Call implements fw.Tool. The run in the context selects the scope. A
// command that writes runs while it holds the lock of the scope
// (memory.Store.Locked), so the read of str_replace and insert and their
// write are one step, and a concurrent write through the native API or of
// another run is never lost.
func (t *memoryTool) Call(ctx context.Context, input json.RawMessage) (fw.ToolResult, error) {
	t.store.GuardToken(fw.AgentFrom(ctx))
	scope := t.store.Scope(fw.RunInfoFrom(ctx))
	if cmd := memoryCommand(input); cmd == "view" || !slices.Contains(memoryCommands, cmd) {
		files, err := t.store.Notes(ctx, scope)
		if err != nil {
			return fw.ErrorResult("Error: " + err.Error()), nil
		}
		res, err := t.ed.call(ctx, files, input)
		if err == nil && !res.IsError && cmd == "view" {
			t.recordView(ctx, scope, input)
		}
		return res, err
	}
	var res fw.ToolResult
	err := t.store.Locked(ctx, scope, func(files fw.Files) error {
		var err error
		if cmd := memoryCommand(input); (cmd == "insert" || cmd == "str_replace") && fw.RunInfoFrom(ctx).CallID != "" {
			res, err = t.editOnce(ctx, files, input)
			return err
		}
		res, err = t.ed.call(ctx, files, input)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx, "memory")
		}
		return fw.ErrorResult("Error: " + err.Error()), nil
	}
	return res, nil
}

// editOnce runs an insert or a str_replace of the call in ctx once. The
// files come from Locked. It looks for an earlier record of the same call
// in the scope: when the note has the hash after that edit, the first call
// wrote, and editOnce returns its result again; when the note has the
// hash before, the first call did not write, and the edit runs; when the
// note has neither, another write came after the first call, and the call
// fails. A new edit runs against a copy of the files that holds back the
// write, records the call id, the hash before, the hash after, and the
// result, and then writes (docs/KNOWHOW.md section 4.3).
func (t *memoryTool) editOnce(ctx context.Context, files fw.Files, input json.RawMessage) (fw.ToolResult, error) {
	callID := fw.RunInfoFrom(ctx).CallID
	var in editInput
	if err := decode(input, &in); err != nil {
		return badInput("memory", err), nil
	}
	name, err := memoryName(in.Path)
	if err != nil || name == "." {
		return t.ed.call(ctx, files, input)
	}
	shown := memoryShow(name)
	before := noteHash(files, name)
	prev, found, err := memory.EditOf(files, callID)
	if err != nil {
		return fw.ToolResult{}, err
	}
	if found && prev.Name == name {
		switch before {
		case prev.After:
			return fw.TextResult(prev.Result), nil
		case prev.Before:
			// The first call recorded its edit and did not write.
		default:
			return fw.ErrorResult("Error: " + shown + " changed after the first run of this call. " + memory.ConflictText), nil
		}
	}
	held := &heldWrite{Files: files}
	res, err := t.ed.call(ctx, held, input)
	if err != nil || res.IsError || !held.done {
		return res, err
	}
	sum := sha256.Sum256(held.data)
	if err := memory.RecordEdit(files, memory.CallEdit{
		CallID: callID, Name: name, Before: before, After: hex.EncodeToString(sum[:]), Result: fw.TextOf(res.Content),
	}); err != nil {
		return fw.ErrorResult("Error: " + err.Error()), nil
	}
	if err := t.ed.write(files, held.name, shown, string(held.data)); err != nil {
		return fw.ErrorResult(err.Error()), nil
	}
	return res, nil
}

// heldWrite is a store that keeps the one write of an edit back, so the
// caller records the edit before the write (editOnce).
type heldWrite struct {
	fw.Files
	name string
	data []byte
	done bool
}

// WriteFile implements fw.Files: it keeps the write for the caller.
func (h *heldWrite) WriteFile(name string, data []byte) error {
	h.name, h.data, h.done = name, append([]byte(nil), data...), true
	return nil
}

// noteHash returns the hex SHA-256 of a note, or "" when it cannot be
// read.
func noteHash(files fs.FS, name string) string {
	data, err := fs.ReadFile(files, name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// recordView records fw.memory.used for a view of one note, with the hash
// of the note. A view of a directory reads no note and records nothing.
func (t *memoryTool) recordView(ctx context.Context, scope fw.MemoryScope, input json.RawMessage) {
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return
	}
	name, err := memoryName(in.Path)
	if err != nil || name == "." {
		return
	}
	rec, err := t.store.Get(ctx, scope, memory.NoteRef(scope, name))
	if err != nil {
		return
	}
	fw.RecordMemoryUse(ctx, fw.MemoryUsedData{How: fw.MemoryUsedView, Refs: []fw.MemoryUsedRef{{Ref: rec.Ref, Hash: rec.Hash}}})
}

// Brief implements fw.Briefer. It returns the memory section of the system
// prompt: how the memory works and the rules for new notes. It holds no
// note, so no text that a run wrote reaches the system prompt.
func (t *memoryTool) Brief(_ context.Context, run fw.RunInfo) (string, error) {
	return t.store.Section(t.store.Scope(run)), nil
}

// BriefContext implements fw.ContextBriefer. It returns the index of the
// notes of the scope of the run, which the kernel puts in the first user
// message as data, and records the notes that it lists as fw.memory.used.
// No note gives no text.
func (t *memoryTool) BriefContext(ctx context.Context, run fw.RunInfo) (string, error) {
	t.store.GuardToken(fw.AgentFrom(ctx))
	b, err := t.store.Brief(ctx, t.store.Scope(run), memory.BriefHint)
	if err != nil {
		return "", fmt.Errorf("builtin: %w", err)
	}
	fw.RecordMemoryUse(ctx, fw.MemoryUsedData{How: fw.MemoryUsedBrief, Refs: b.Used})
	return b.Text, nil
}

// memoryName maps /memories/<rest> to <rest>, relative to the memory
// directory, with slashes. A name with a hidden part is refused.
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
	name := filepath.ToSlash(filepath.Clean(rest))
	if err := memory.CheckName(name); err != nil {
		return "", err
	}
	return name, nil
}

// memoryShow maps a name of the memory directory to /memories/...
func memoryShow(name string) string {
	if name == "." || name == "" {
		return memoryPrefix
	}
	return memoryPrefix + "/" + name
}

package memory

// This file lets a run of the principal scope read the notes of the agent
// scope of its agent (docs/KNOWHOW.md section 4.2). The after-work review
// keeps its notes in the agent scope, so a pool whose workers set
// tools.memory.scope: principal reads them this way. The run reads them
// and never changes them: its own writes go to its principal scope.
//
// The model sees the notes of the agent under /memories/.agent. A part of
// a name that starts with '.' is never a note (CheckName): the store, the
// native API, and the pull of the front refuse such a name, and the walks
// skip it. So no note of the principal can take that path, and the notes
// of the agent never hide a note of the principal.

import (
	"errors"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// AgentDir is the directory under Prefix where a run of the principal
// scope reads the notes of the agent scope of its agent.
const AgentDir = ".agent"

// AgentPrefix is the path that a model uses for the notes of the agent.
const AgentPrefix = Prefix + "/" + AgentDir

// The lines of the block of the notes before each group, when a run of
// the principal scope has notes of the agent to list (Brief).
const (
	AgentNotesHead = "Notes of this agent, read-only:"
	OwnNotesHead   = "Your own notes:"
)

// AgentSection is the sentence that the memory section of the system
// prompt adds for a run of the principal scope (Section).
const AgentSection = "\n\nThe notes under " + AgentPrefix + " are the notes of this agent, and every run of the agent reads them. " +
	"When the list at the start of a run holds some, it names them first. " +
	"This run can read them but cannot change them. To correct one, keep a note of your own under " + Prefix + "."

// AgentScope returns the agent scope that a run of scope also reads: the
// notes of the agent scope of its agent, read-only. ok is false for
// every scope but the principal scope, so the scopes agent and shared
// keep their behavior.
func AgentScope(scope fw.MemoryScope) (fw.MemoryScope, bool) {
	if scope.Kind != fw.MemoryPrincipalScope {
		return fw.MemoryScope{}, false
	}
	return fw.MemoryScope{Kind: fw.MemoryAgentScope, Agent: scope.Agent, Task: scope.Task}, true
}

// AgentName reports whether name, a clean slash-separated path relative
// to Prefix, such as ".agent/gitea.md", names the notes of the agent. It
// returns the name of the note in the agent scope, or "." for AgentDir
// itself. The caller checks the name with CheckName.
func AgentName(name string) (string, bool) {
	if name == AgentDir {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(name, AgentDir+"/"); ok {
		return rest, true
	}
	return "", false
}

// AgentReadOnly is the answer of a memory tool to a change of a note of the
// agent from a run of the principal scope. name is the note in the agent
// scope, or "." or "" for the directory.
func AgentReadOnly(name string) string {
	s := "Error: the notes under " + AgentPrefix + " belong to this agent, so this run can read them but cannot change them. " +
		"Keep your own note instead"
	if name != "" && name != "." {
		s += ", such as " + Prefix + "/" + name
	}
	return s + "."
}

// ShowRef returns the path under Prefix of a note ref that a run of scope
// reads: /memories/<name> for a note of the scope, /memories/.agent/<name>
// for a note of the agent that the scope reads, and the ref itself for
// any other ref.
func ShowRef(scope fw.MemoryScope, ref string) string {
	if name, ok := strings.CutPrefix(ref, NoteRef(scope, "")); ok && name != "" {
		return Prefix + "/" + name
	}
	if agent, ok := AgentScope(scope); ok {
		if name, ok := strings.CutPrefix(ref, NoteRef(agent, "")); ok && name != "" {
			return AgentPrefix + "/" + name
		}
	}
	return ref
}

// readOnly is the store of the notes of the agent for a run of the
// principal scope: it reads, and every write fails.
type readOnly struct{ fw.Files }

// errAgentNote is the error of a write to the notes of the agent from a
// run of the principal scope.
var errAgentNote = errors.New("the notes of the agent are read-only in this run")

func (readOnly) WriteFile(string, []byte) error { return errAgentNote }
func (readOnly) Remove(string) error            { return errAgentNote }
func (readOnly) Rename(string, string) error    { return errAgentNote }

// AgentNotes returns the notes of the agent that a run of scope reads,
// read-only: every write fails. ok is false for every scope but the
// principal scope. It creates nothing: a missing directory has no notes.
func (s *Store) AgentNotes(scope fw.MemoryScope) (files fw.Files, agent fw.MemoryScope, ok bool, err error) {
	agent, ok = AgentScope(scope)
	if !ok {
		return nil, fw.MemoryScope{}, false, nil
	}
	dir, err := s.Dir(agent)
	if err != nil {
		return nil, agent, true, err
	}
	inner, err := fw.DirFiles(dir)
	if err != nil {
		return nil, agent, true, err
	}
	return readOnly{inner}, agent, true, nil
}

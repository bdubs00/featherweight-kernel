package native

// This file serves the memory of the agents (docs/ARCHITECTURE.md section
// 7.8): a search over notes, runs, and skills, and the notes themselves.
// The scope of a request comes from the agent that the request names and
// the principal that signed it, never from a field of the request, so a
// caller reads and writes only the notes that a run of that caller reads
// and writes. Each write and each removal goes to the log with the subject
// of the caller, the operation, the ref, the scope, the size, and the hash
// of the note, never its text, because no run records it in a ledger.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

// memoryRoutes are the routes of the memory.
func (s *server) memoryRoutes() []struct {
	pattern string
	h       http.HandlerFunc
} {
	return []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"GET /v1/memory", s.searchMemory},
		{"GET /v1/memory/notes", s.listNotes},
		{"GET /v1/memory/notes/{path...}", s.getNote},
		{"PUT /v1/memory/notes/{path...}", s.putNote},
		{"DELETE /v1/memory/notes/{path...}", s.deleteNote},
	}
}

// memoryScope checks the agent of the request and the action of the caller
// on it, and returns the scope of the caller in that agent.
//
// An agent that does not exist and an agent that the caller cannot view
// get the same answer, 404 with the same text, as GET /v1/agents/{name}
// gives, so the routes do not tell which agents exist. A caller who can
// view the agent and lacks the action gets 403.
//
// Reading needs the right to run the agent. Writing a note of the principal
// scope needs the right to run the agent too. Writing a note of the agent
// scope, which every person of the agent reads, needs the right to edit the
// agent. Writing a note of the shared scope, which every profile of the
// process reads, needs the admin role of the process (access.admins).
func (s *server) memoryScope(w http.ResponseWriter, r *http.Request, write bool) (fw.MemoryScope, bool) {
	if s.mem == nil {
		problem(w, r, http.StatusServiceUnavailable, "the memory is not available: "+s.memErr.Error())
		return fw.MemoryScope{}, false
	}
	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		agent = "default"
	}
	unknown := func() {
		problem(w, r, http.StatusNotFound, "no agent profile has the name "+strconv.Quote(agent))
	}
	if !memory.ValidAgentName(agent) {
		unknown()
		return fw.MemoryScope{}, false
	}
	if _, real := s.a.ProfileNamed(agent); real {
		switch err := s.a.Authorize(r.Context(), fw.ActionAgentView, fw.Target{Agent: agent}); {
		case errors.Is(err, fw.ErrForbidden):
			unknown()
			return fw.MemoryScope{}, false
		case err != nil:
			s.fail(w, r, err)
			return fw.MemoryScope{}, false
		}
	} else if agent != "default" {
		unknown()
		return fw.MemoryScope{}, false
	}
	info := fw.RunInfo{Agent: agent}
	if p := fw.PrincipalFrom(r.Context()); p != nil {
		info.Principal = p.Subject
	}
	scope := s.mem.Scope(info)
	action, target := fw.ActionAgentRun, fw.Target{Agent: agent}
	if write {
		switch scope.Kind {
		case fw.MemoryAgentScope:
			action = fw.ActionAgentEdit
		case fw.MemorySharedScope:
			action, target = fw.ActionAdmin, fw.Target{}
		}
	}
	if err := s.a.Authorize(r.Context(), action, target); err != nil {
		if action == fw.ActionAdmin && errors.Is(err, fw.ErrForbidden) {
			problem(w, r, http.StatusForbidden, "a note of the shared scope reaches every profile of this process, so writing or removing one needs the admin role of the process (access.admins)")
			return fw.MemoryScope{}, false
		}
		s.fail(w, r, err)
		return fw.MemoryScope{}, false
	}
	return scope, true
}

// callerOf returns the subject of the caller of a request, for a log line,
// or "" for a call of the process itself.
func callerOf(r *http.Request) string {
	if p := fw.PrincipalFrom(r.Context()); p != nil {
		return p.Subject
	}
	return ""
}

// searchMemory answers GET /v1/memory?agent=&q=&kinds=&limit= with the
// records that match q: notes of the scope of the caller, runs of the agent
// that the caller started, and skills. A search over the files of a
// workspace belongs to a run, so this route refuses the kind file.
func (s *server) searchMemory(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.memoryScope(w, r, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))
	if text == "" {
		problem(w, r, http.StatusBadRequest, "the query parameter q must hold the words to find")
		return
	}
	var names []string
	if v := q.Get("kinds"); v != "" {
		names = strings.Split(v, ",")
	}
	kinds, err := memory.ParseKinds(names)
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if len(kinds) == 0 {
		kinds = []fw.MemoryKind{fw.MemoryKindNote, fw.MemoryKindRun, fw.MemoryKindSkill}
	}
	if slices.Contains(kinds, fw.MemoryKindFile) {
		problem(w, r, http.StatusBadRequest, "the kind file is searched by the recall tool of a run, not by this route")
		return
	}
	limit := memory.DefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = min(n, memory.MaxLimit)
	}
	res, err := s.mem.Recall(fw.WithAgent(r.Context(), s.a), scope, fw.MemoryQuery{Text: text, Kinds: kinds, Limit: limit})
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if res.Hits == nil {
		res.Hits = []fw.MemoryHit{}
	}
	out := map[string]any{"scope": scope.Kind, "hits": res.Hits}
	if len(res.Notes) > 0 {
		out["notes"] = res.Notes
	}
	writeJSON(w, http.StatusOK, out)
}

// listNotes answers GET /v1/memory/notes?agent= with the notes of the
// scope of the caller, newest first, without their text. cut true says
// that the walk stopped at its bound, so notes can be missing.
func (s *server) listNotes(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.memoryScope(w, r, false)
	if !ok {
		return
	}
	notes, cut, err := s.mem.List(r.Context(), scope)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if notes == nil {
		notes = []fw.MemoryRecord{}
	}
	out := map[string]any{"scope": scope.Kind, "notes": notes}
	if cut {
		out["cut"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

// noteRef returns the ref of the note that the path of the request names,
// or writes the problem.
func noteRef(w http.ResponseWriter, r *http.Request, scope fw.MemoryScope) (string, bool) {
	name := r.PathValue("path")
	if err := memory.CheckName(name); err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return "", false
	}
	return memory.NoteRef(scope, name), true
}

// getNote answers GET /v1/memory/notes/{path}?agent= with one note and its
// hash. A client that accepts text/markdown or text/plain and not JSON
// gets the text alone, with the hash in the header ETag.
func (s *server) getNote(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.memoryScope(w, r, false)
	if !ok {
		return
	}
	ref, ok := noteRef(w, r, scope)
	if !ok {
		return
	}
	rec, err := s.mem.Get(r.Context(), scope, ref)
	if err != nil {
		s.failMemory(w, r, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(rec.Hash))
	if !accepts(r, "application/json") && (accepts(r, "text/markdown") || accepts(r, "text/plain")) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rec.Body)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// putNote answers PUT /v1/memory/notes/{path}?agent= by writing the body,
// the text of the note, as the note. The header If-Match with the hash of
// the current note makes the write a compare-and-swap, If-Match: * writes
// only a note that exists, and If-None-Match: * only a new note; each
// answers 412 when the note differs. The answer is the note with its new
// hash.
func (s *server) putNote(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.memoryScope(w, r, true)
	if !ok {
		return
	}
	ref, ok := noteRef(w, r, scope)
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, memory.MaxNoteBytes))
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		problem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("a note holds at most %d KB", memory.MaxNoteBytes>>10))
		return
	case err != nil:
		problem(w, r, http.StatusBadRequest, "the body cannot be read: "+err.Error())
		return
	}
	ifHash := ""
	switch m := strings.TrimSpace(r.Header.Get("If-Match")); m {
	case "":
	case "*":
		ifHash = "present"
	default:
		ifHash = strings.Trim(strings.TrimPrefix(m, "W/"), `"`)
	}
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" {
		ifHash = "absent"
	}
	// No run records this write in a ledger, so the change that the store
	// records goes to the log, with the caller.
	var change fw.MemoryChangedData
	ctx := fw.WithMemoryRecorder(r.Context(), func(d fw.MemoryChangedData) { change = d })
	rec, err := s.mem.Remember(ctx, scope, fw.MemoryRecord{Ref: ref, Body: string(body)}, ifHash)
	if err != nil {
		s.failMemory(w, r, err)
		return
	}
	s.a.Logger().Info("native: memory note written", "caller", callerOf(r), "op", change.Op, "agent", scope.Agent, "scope", scope.Kind,
		"ref", rec.Ref, "size", change.Size, "sha256", change.SHA256)
	w.Header().Set("ETag", strconv.Quote(rec.Hash))
	writeJSON(w, http.StatusOK, rec)
}

// deleteNote answers DELETE /v1/memory/notes/{path}?agent= by removing the
// note.
func (s *server) deleteNote(w http.ResponseWriter, r *http.Request) {
	scope, ok := s.memoryScope(w, r, true)
	if !ok {
		return
	}
	ref, ok := noteRef(w, r, scope)
	if !ok {
		return
	}
	gone, err := s.mem.ForgetNote(r.Context(), scope, ref)
	if err != nil {
		s.failMemory(w, r, err)
		return
	}
	s.a.Logger().Info("native: memory note removed", "caller", callerOf(r), "op", fw.FileRemoved, "agent", scope.Agent, "scope", scope.Kind,
		"ref", gone.Ref, "size", gone.Size, "sha256", gone.Hash)
	w.WriteHeader(http.StatusNoContent)
}

// failMemory maps an error of the memory to a problem document.
func (s *server) failMemory(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fw.ErrMemoryNotFound):
		problem(w, r, http.StatusNotFound, "no note has the path "+strconv.Quote(r.PathValue("path")))
	case errors.Is(err, memory.ErrFrontConflict):
		problem(w, r, http.StatusPreconditionFailed, memory.ConflictText)
	case errors.Is(err, memory.ErrNotKept):
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem(w, r, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, fw.ErrMemoryConflict):
		problem(w, r, http.StatusPreconditionFailed, "the note changed: its hash differs from If-Match, or it exists and If-None-Match is *")
	case errors.Is(err, memory.ErrLimit):
		problem(w, r, http.StatusRequestEntityTooLarge, err.Error())
	default:
		s.fail(w, r, err)
	}
}

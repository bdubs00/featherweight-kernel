package native

// This file holds the management routes: who the caller is, the agent
// profiles that the caller owns, and a page of a run's ledger. The routes
// for people, such as the home and the result of a run, come from the
// workbench through an extension (see extend.go).

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// Limits of a page of events.
const (
	defaultEventsLimit = 200
	maxEventsLimit     = 1000
)

// whoami answers GET /v1/whoami with the principal, its role on every
// agent it can see, and whether it is an admin or can create agents.
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	who, err := view.WhoamiOf(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, who)
}

// status answers GET /v1/status with the state of the agent at a glance:
// the usage of the runs that the caller can view, the oldest approval that
// waits, and the counts of runs by state. A control plane polls it, and
// under ax the workbench reads it through the atenet router.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st, err := view.StatusOf(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// getAgent answers GET /v1/agents/{name} with one profile, the role of the
// caller, and the usage of its visible runs.
func (s *server) getAgent(w http.ResponseWriter, r *http.Request) {
	v, err := view.Agent(r.Context(), s.a, r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// putAgent answers PUT /v1/agents/{name}. The body is an fw.AgentProfile.
// A body that names another profile is refused. A new profile answers 201,
// and a replaced one 200, both with the saved profile. With ?dryRun=true,
// it checks the profile, saves nothing, and answers 200 with a
// view.Draft.
func (s *server) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var p fw.AgentProfile
	if !s.decode(w, r, &p) {
		return
	}
	if p.Name != "" && p.Name != name {
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the body names the profile %q but the path names %q", p.Name, name))
		return
	}
	p.Name = name
	if r.URL.Query().Get("dryRun") == "true" {
		d, err := view.Preview(r.Context(), s.a, p)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
		return
	}
	v, created, err := view.Put(r.Context(), s.a, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if created {
		w.Header().Set("Location", "/v1/agents/"+name)
		writeJSON(w, http.StatusCreated, v)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// deleteAgent answers DELETE /v1/agents/{name} with 204.
func (s *server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.a.DeleteProfile(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// eventsPage answers GET /v1/runs/{id}/events for a client that accepts
// JSON: the run object and the events from the sequence number from, at
// most limit of them, as {"run": ..., "events": [...], "lines": [...]}.
// lines holds each event as the short line that a person reads (see
// view.Line), the same lines as the MCP tool run_events. Without
// from, the page holds the last limit events.
func (s *server) eventsPage(w http.ResponseWriter, r *http.Request, id string, from uint64) {
	run, err := s.a.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit := defaultEventsLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = min(n, maxEventsLimit)
	}
	var events []fw.Event
	for e, rerr := range s.a.Ledger().Read(r.Context(), id, from) {
		if rerr != nil {
			s.fail(w, r, fmt.Errorf("native: read the ledger of run %s: %w", id, rerr))
			return
		}
		events = append(events, e)
		if from > 0 && len(events) >= limit {
			break
		}
	}
	if from == 0 && len(events) > limit {
		events = events[len(events)-limit:]
	}
	if events == nil {
		events = []fw.Event{}
	}
	lines := make([]view.EventLine, 0, len(events))
	for _, e := range events {
		lines = append(lines, view.Line(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "events": events, "lines": lines})
}

// retryAfterSeconds is the Retry-After of a 503 for a control plane that
// did not answer.
const retryAfterSeconds = 5

// unavailableDetail is the detail of a 503 problem when the authorization
// service did not answer. It quotes the cause, which names the decision
// point and never a token.
func unavailableDetail(err error) string {
	cause := err.Error()
	if _, after, ok := strings.Cut(cause, fw.ErrAuthzUnavailable.Error()); ok {
		cause = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), ":"))
	}
	if cause == "" {
		return "the authorization service did not answer, so the request is refused; try again in a few seconds"
	}
	return "the authorization service did not answer, so the request is refused; try again in a few seconds: " + cause
}

// forbiddenDetail is the detail of a 403 problem: who is missing which
// role, as the authorization rules said it.
func forbiddenDetail(err error) string {
	detail := strings.TrimSpace(strings.TrimPrefix(err.Error(), fw.ErrForbidden.Error()))
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
	if detail == "" {
		return "the caller is not allowed to do this"
	}
	return detail
}

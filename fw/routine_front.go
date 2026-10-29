package fw

// This file gives each run the routines of its agent from the front of a
// fleet (docs/ROUTINES.md section 3.6), when routines.front is set. The
// front keeps the routines in a store, and a move of a routine, such as
// rehearsing to live, changes no configuration of a worker: the run reads
// the routines at its start, the way it reads its notes. The client keeps
// the list of each agent with its version and asks the front each time
// with that version, so a copy is applied only when the front says that it
// is current. A front that does not answer gives the run no routine,
// because the agent doing the work is the safe direction. The run records
// what it read in fw.run.started, so the runner, a restart, and the
// comparison at the end read the same list.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bounds of the reads of the routines.
const (
	// RoutinesWait bounds the read of the routines at the start of a run.
	RoutinesWait = 5 * time.Second
	// maxRoutinesBody bounds the answer of the front.
	maxRoutinesBody = 4 << 20
	// RoutinesPath is the route of the front that answers the routines of
	// an agent now, with ?agent=.
	RoutinesPath = "/v1/routines/now"
)

// RoutinesConfig says where the routines of the agents come from. Front
// names the front of a fleet that keeps them; nil means that each profile
// carries its own (AgentProfile.Routines).
type RoutinesConfig struct {
	Front *MemoryFrontConfig `yaml:"front,omitempty" json:"front,omitempty"`
}

// RoutineList is what a RoutineSource answers for one agent. Held is true
// when the source keeps the routines of the agent: List then replaces the
// routines of the profile, also when it is empty. Version names the list;
// the same version always names the same list.
type RoutineList struct {
	Held    bool      `json:"held"`
	Version string    `json:"version,omitempty"`
	List    []Routine `json:"routines"`
}

// RoutineSource gives a run the routines of its agent at the start of the
// run. An error means that the source did not answer: the run then
// applies no routine. routines.front builds one, and a test gives one with
// WithRoutineSource.
type RoutineSource interface {
	RoutinesOf(ctx context.Context, agent string) (RoutineList, error)
}

// StartRoutines is the field routines of fw.run.started: the routines that
// the source gave the run at its start, for a run whose agent the source
// holds (RoutineList.Held), with the version. Error says why the source
// gave none, and List is then empty: the run applies no routine.
type StartRoutines struct {
	Version string    `json:"version,omitempty"`
	List    []Routine `json:"list"`
	Error   string    `json:"error,omitempty"`
}

// frontRoutines is the RoutineSource of routines.front: the client of the
// front with the last list of each agent.
type frontRoutines struct {
	base     *url.URL
	tokenEnv string
	client   *http.Client

	mu    sync.Mutex
	cache map[string]RoutineList // by agent
}

// newFrontRoutines returns the source of the front that cfg names, or nil
// when it names none.
func newFrontRoutines(cfg *MemoryFrontConfig) (*frontRoutines, error) {
	if cfg == nil || strings.TrimSpace(cfg.URL) == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: routines.front.url %q is not an http or https address", ErrInvalidRequest, cfg.URL)
	}
	if strings.TrimSpace(cfg.TokenEnv) == "" {
		return nil, fmt.Errorf("%w: routines.front.tokenEnv must name the variable that holds the static token of the front", ErrInvalidRequest)
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = strings.TrimRight(u.Path, "/"), "", "", ""
	return &frontRoutines{base: u, tokenEnv: strings.TrimSpace(cfg.TokenEnv), client: &http.Client{}, cache: map[string]RoutineList{}}, nil
}

// RoutinesOf asks the front for the routines of the agent, with the
// version of the list that the client holds. The front answers 304 when
// that list is current, and the client gives it; else it answers the list
// with its version, which the client keeps. Any other answer is an error,
// and the client gives no list. An error never holds the address or the
// token.
func (f *frontRoutines) RoutinesOf(ctx context.Context, agent string) (RoutineList, error) {
	f.mu.Lock()
	held, cached := f.cache[agent]
	f.mu.Unlock()
	target := f.base.String() + RoutinesPath + "?" + url.Values{"agent": {agent}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return RoutineList{}, errors.New("the request to the front cannot be built")
	}
	req.Header.Set("Accept", "application/json")
	if cached && held.Version != "" {
		req.Header.Set("If-None-Match", strconv.Quote(held.Version))
	}
	if tok := os.Getenv(f.tokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return RoutineList{}, errors.New("the front did not answer in time")
		}
		return RoutineList{}, errors.New("the front did not answer")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRoutinesBody))
	switch {
	case resp.StatusCode == http.StatusNotModified && cached:
		return held, nil
	case resp.StatusCode != http.StatusOK:
		return RoutineList{}, fmt.Errorf("the front answered %d", resp.StatusCode)
	case err != nil:
		return RoutineList{}, errors.New("the answer of the front was cut off")
	}
	var out RoutineList
	if err := json.Unmarshal(bytes.TrimSpace(data), &out); err != nil {
		return RoutineList{}, errors.New("the answer of the front is not the JSON that the kernel expects")
	}
	if out.Version == "" {
		out.Version = strings.Trim(resp.Header.Get("ETag"), `"`)
	}
	if out.List == nil {
		out.List = []Routine{}
	}
	f.mu.Lock()
	f.cache[agent] = out
	f.mu.Unlock()
	return out, nil
}

// startRoutines reads the routines of a new run from the source of the
// agent, for fw.run.started. It returns nil when the agent has no source,
// when the run is one whose routines never run (a check run, a branch, a
// replay, or a run that is held back from Know-how), and when the source
// does not hold the agent: the run then takes the routines of its profile.
// A source that does not answer within RoutinesWait, or a list that the
// kernel does not accept (CheckRoutines), gives the run no routine, and
// the field says why.
func (a *Agent) startRoutines(ctx context.Context, rs *runState, q RunQueuedData) *StartRoutines {
	src := a.routineSrc
	if src == nil || q.Check != nil || q.Kept > 0 || HeldBack(q.Metadata) || q.Metadata["replayOf"] != "" || a.k.override(rs.id) != nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, RoutinesWait)
	defer cancel()
	l, err := src.RoutinesOf(rctx, q.Agent)
	if err != nil {
		a.log.Warn("routines: the front gave no routines; the run applies none", "run", rs.id, "agent", q.Agent, "error", err)
		return &StartRoutines{List: []Routine{}, Error: err.Error()}
	}
	if !l.Held {
		return nil
	}
	if err := CheckRoutines(a.cfg, l.List); err != nil {
		a.log.Warn("routines: the routines of the front do not fit this process; the run applies none", "run", rs.id, "agent", q.Agent, "error", err)
		return &StartRoutines{Version: l.Version, List: []Routine{}, Error: err.Error()}
	}
	return &StartRoutines{Version: l.Version, List: append([]Routine{}, l.List...)}
}

// routinesOf returns the routines of a run: those that fw.run.started
// recorded from the source of the agent (startRoutines), else those of its
// profile, which the kernel pinned at the queue of the run.
func (a *Agent) routinesOf(c *cursor) []Routine {
	if r := c.routine.start; r != nil {
		return r.List
	}
	return a.profileOf(c.queued).Routines
}

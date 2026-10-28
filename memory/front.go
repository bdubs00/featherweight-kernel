package memory

// This file is the client of the front of a fleet, which keeps the notes
// when tools.memory.front is set (docs/KNOWHOW.md sections 4.2 and 5.3).
// The files under $FW_HOME/memory are then a copy. The client reads the
// static token from the variable of the environment that tokenEnv names,
// for each request, and sends it only in the header Authorization. No
// error of the client holds the token or the address of a request, which
// names the principal.

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
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of the requests to the front.
const (
	// PullWait bounds the pull at the start of a run. A front that does
	// not answer within it leaves the run with the copy.
	PullWait = 5 * time.Second
	// pushWait bounds one change at the front. The store holds the lock of
	// the scope while it waits.
	pushWait = 10 * time.Second
	// maxFrontBody bounds the body of an answer of the front.
	maxFrontBody = 4 << 20
)

// The states of a note at the front (docs/KNOWHOW.md section 5.1).
const (
	StateChecked   = "checked"
	StateUnchecked = "unchecked"
	StateOutOfDate = "out-of-date"
	StateResting   = "resting"
	StatePutAway   = "put-away"
	StateWaiting   = "waiting"
)

// ErrFrontConflict is the error of a change that the front refused with
// 412, because the note changed in another run. The store has put the
// text of the front back in the copy. It is fw.ErrMemoryConflict too.
var ErrFrontConflict error = frontConflict{}

type frontConflict struct{}

func (frontConflict) Error() string        { return ConflictText }
func (frontConflict) Is(target error) bool { return target == fw.ErrMemoryConflict }

// ErrNotKept is the error of a change that the front did not take: it did
// not answer, or it refused the change. The store has undone the change in
// the copy.
var ErrNotKept = errors.New("the front of the fleet did not take the change, so the note was not kept")

// front is the client of the front of a fleet.
type front struct {
	base     *url.URL
	tokenEnv string
	client   *http.Client
}

// newFront returns the client of the front that cfg names, or nil when cfg
// names none.
func newFront(cfg *fw.MemoryFrontConfig) (*front, error) {
	if cfg == nil || strings.TrimSpace(cfg.URL) == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("memory: tools.memory.front.url %q is not an http or https address", cfg.URL)
	}
	if strings.TrimSpace(cfg.TokenEnv) == "" {
		return nil, errors.New("memory: tools.memory.front.tokenEnv must name the variable that holds the static token of the front")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = strings.TrimRight(u.Path, "/"), "", "", ""
	return &front{base: u, tokenEnv: strings.TrimSpace(cfg.TokenEnv), client: &http.Client{}}, nil
}

// GuardToken makes the agent replace the static token of the front in
// every later ledger event and in Redact, whatever the name of the
// variable that holds it. Each holder of a Store calls it once it has the
// agent. Without a front it does nothing.
func (s *Store) GuardToken(a *fw.Agent) {
	if s == nil || s.front == nil || a == nil {
		return
	}
	if tok := os.Getenv(s.front.tokenEnv); tok != "" {
		a.AddSecret(tok)
	}
}

// frontScope reports whether the front keeps the notes of a scope: the
// scopes agent and principal. The shared scope of a process stays in the
// files of the process.
func frontScope(scope fw.MemoryScope) bool {
	return scope.Kind == fw.MemoryAgentScope || scope.Kind == fw.MemoryPrincipalScope
}

// FrontNote is one note of the list of the front.
type FrontNote struct {
	Name        string    `json:"name"`
	Hash        string    `json:"hash"`
	State       string    `json:"state,omitempty"`
	Description string    `json:"description,omitempty"`
	By          string    `json:"by,omitempty"`
	At          time.Time `json:"at,omitzero"`
	Uses        int       `json:"uses,omitempty"`
	Sources     int       `json:"sources,omitempty"` // the count of the sources
}

// UnmarshalJSON reads a note of the list. The field sources is a count or
// a list, and a time that does not parse is no time.
func (n *FrontNote) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name        string          `json:"name"`
		Hash        string          `json:"hash"`
		State       string          `json:"state"`
		Description string          `json:"description"`
		By          string          `json:"by"`
		At          string          `json:"at"`
		Uses        int             `json:"uses"`
		Sources     json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*n = FrontNote{Name: raw.Name, Hash: raw.Hash, State: raw.State, Description: raw.Description, By: raw.By, Uses: raw.Uses}
	n.At, _ = time.Parse(time.RFC3339Nano, raw.At)
	var count int
	var list []json.RawMessage
	switch {
	case json.Unmarshal(raw.Sources, &count) == nil:
		n.Sources = count
	case json.Unmarshal(raw.Sources, &list) == nil:
		n.Sources = len(list)
	}
	return nil
}

// kept reports whether the copy holds the note: every state but put away
// and waiting.
func (n FrontNote) kept() bool { return n.State != StatePutAway && n.State != StateWaiting }

// frontSource is one source of a note on the wire of the front.
type frontSource struct {
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

// frontRecord is one note with its text (GET of one note).
type frontRecord struct {
	Name    string        `json:"name"`
	Text    string        `json:"text"`
	Hash    string        `json:"hash"`
	State   string        `json:"state"`
	Sources []frontSource `json:"sources"`
}

// frontPut is the body of a change of a note.
type frontPut struct {
	Text    string        `json:"text"`
	IfHash  string        `json:"ifHash"`
	Sources []frontSource `json:"sources"`
	By      string        `json:"by"`
	Run     string        `json:"run,omitempty"`
}

// FrontRun is one run of the archive of the front (GET /v1/knowhow/runs).
// The front cuts each text to 2 KB.
type FrontRun struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Input   string    `json:"input"`
	Output  string    `json:"output"`
	At      time.Time `json:"at,omitzero"`
	Outcome string    `json:"outcome"`
}

// errStatus is the error of an answer with a status that the caller does
// not expect.
type errStatus struct{ code int }

func (e errStatus) Error() string { return "the front answered " + strconv.Itoa(e.code) }

// do sends one request and decodes a JSON answer into out, when out is
// not nil. It returns the status. An error never holds the address or the
// token.
func (f *front) do(ctx context.Context, method, path string, q url.Values, body, out any) (int, error) {
	target := f.base.String() + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, errors.New("the request to the front cannot be built")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := os.Getenv(f.tokenEnv); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, errors.New("the front did not answer in time")
		}
		return 0, fmt.Errorf("the front did not answer: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFrontBody))
	if err != nil {
		return resp.StatusCode, errors.New("the answer of the front was cut off")
	}
	if out != nil && resp.StatusCode/100 == 2 && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, errors.New("the answer of the front is not the JSON that the kernel expects")
		}
	}
	return resp.StatusCode, nil
}

// scopeQuery returns the query of a scope: the agent, the kind of the
// scope, and for the principal scope the subject of the principal.
func scopeQuery(scope fw.MemoryScope) url.Values {
	q := url.Values{}
	q.Set("agent", scope.Agent)
	q.Set("scope", string(scope.Kind))
	if scope.Kind == fw.MemoryPrincipalScope {
		q.Set("principal", scope.Principal)
	}
	return q
}

// notePath returns the route of one note: /v1/knowhow/notes/{agent}/{scope}/{name...},
// each part of the name escaped.
func notePath(scope fw.MemoryScope, name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/v1/knowhow/notes/" + url.PathEscape(scope.Agent) + "/" + string(scope.Kind) + "/" + strings.Join(parts, "/")
}

// noteQuery returns the query of one note: the subject of the principal of
// a principal scope.
func noteQuery(scope fw.MemoryScope) url.Values {
	q := url.Values{}
	if scope.Kind == fw.MemoryPrincipalScope {
		q.Set("principal", scope.Principal)
	}
	return q
}

// list returns the notes of a scope at the front.
func (f *front) list(ctx context.Context, scope fw.MemoryScope) ([]FrontNote, error) {
	var out struct {
		Notes []FrontNote `json:"notes"`
	}
	code, err := f.do(ctx, http.MethodGet, "/v1/knowhow/notes", scopeQuery(scope), nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errStatus{code}
	}
	return out.Notes, nil
}

// get returns one note of the front with its text. found is false when the
// front has no such note.
func (f *front) get(ctx context.Context, scope fw.MemoryScope, name string) (rec frontRecord, found bool, err error) {
	code, err := f.do(ctx, http.MethodGet, notePath(scope, name), noteQuery(scope), nil, &rec)
	switch {
	case err != nil:
		return frontRecord{}, false, err
	case code == http.StatusNotFound || code == http.StatusGone:
		return frontRecord{}, false, nil
	case code != http.StatusOK:
		return frontRecord{}, false, errStatus{code}
	}
	if rec.State == StatePutAway {
		return frontRecord{}, false, nil
	}
	return rec, true, nil
}

// put sends the text of a note. ifHash is the hash of the copy before the
// change, or "absent" for a new note. A 412 is ErrFrontConflict.
func (f *front) put(ctx context.Context, scope fw.MemoryScope, name string, body frontPut) error {
	code, err := f.do(ctx, http.MethodPut, notePath(scope, name), noteQuery(scope), body, nil)
	switch {
	case err != nil:
		return err
	case code == http.StatusPreconditionFailed:
		return ErrFrontConflict
	case code/100 != 2:
		return errStatus{code}
	}
	return nil
}

// remove puts a note away at the front. A note that the front does not
// have is gone already. A 412 is ErrFrontConflict.
func (f *front) remove(ctx context.Context, scope fw.MemoryScope, name, ifHash string) error {
	q := noteQuery(scope)
	q.Set("ifHash", ifHash)
	code, err := f.do(ctx, http.MethodDelete, notePath(scope, name), q, nil, nil)
	switch {
	case err != nil:
		return err
	case code == http.StatusPreconditionFailed:
		return ErrFrontConflict
	case code == http.StatusNotFound || code == http.StatusGone:
		return nil
	case code/100 != 2:
		return errStatus{code}
	}
	return nil
}

// runs returns the runs of the archive of the front that match the words
// q, for the agent and the principal of a scope.
func (f *front) runs(ctx context.Context, scope fw.MemoryScope, q string, limit int) ([]FrontRun, error) {
	v := url.Values{}
	v.Set("agent", scope.Agent)
	v.Set("principal", scope.Principal)
	v.Set("q", q)
	v.Set("limit", strconv.Itoa(limit))
	var out struct {
		Runs []FrontRun `json:"runs"`
	}
	code, err := f.do(ctx, http.MethodGet, "/v1/knowhow/runs", v, nil, &out)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, errStatus{code}
	}
	return out.Runs, nil
}

// sourcesOf returns the links of a record as the sources of a change.
func sourcesOf(links []fw.MemoryLink) []frontSource {
	out := []frontSource{}
	for _, l := range links {
		if strings.TrimSpace(l.Ref) != "" {
			out = append(out, frontSource{Ref: l.Ref, Version: l.Version})
		}
	}
	return out
}

// linksOf returns the sources of a note of the front as links.
func linksOf(sources []frontSource) []fw.MemoryLink {
	var out []fw.MemoryLink
	for _, s := range sources {
		out = append(out, fw.MemoryLink{Ref: s.Ref, Kind: "source", Version: s.Version})
	}
	return out
}

// byOf returns who makes a change: "agent" for a call of a run, and
// "person:<subject>" for a person through the native API. The kernel
// builds it from ctx, never from the input of a call, so a model cannot
// set it.
func byOf(ctx context.Context) (by, run string) {
	if id := fw.RunInfoFrom(ctx).RunID; id != "" {
		return "agent", id
	}
	subject := fw.ActingFor(ctx)
	if subject == "" {
		if p := fw.PrincipalFrom(ctx); p != nil {
			subject = p.Subject
		}
	}
	if subject == "" {
		return "person", ""
	}
	return "person:" + subject, ""
}

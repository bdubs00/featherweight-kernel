// Package fronttest is a fake front of a fleet for the tests of the
// kernel: the routes of the notes and of the runs of docs/KNOWHOW.md
// section 5.3, and GET of a page of the wiki, in memory, over httptest. It
// checks the static token and the hash of each change as the front does,
// records each change that it takes, and can stop answering. With ask
// first on, it holds each change of the agent for a person, as the front
// does, and answers 202. It is not a front: it keeps no journal and no
// archive beyond the runs that a test gives it.
package fronttest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source is one source of a note.
type Source struct {
	Ref     string `json:"ref"`
	Version string `json:"version,omitempty"`
}

// Note is one note of the fake front.
type Note struct {
	Agent, Scope, Principal, Name string
	Text                          string
	State                         string
	By                            string
	Run                           string
	At                            time.Time
	Uses                          int
	Sources                       []Source
}

// Hash returns the hex SHA-256 of the text of the note.
func (n Note) Hash() string { return Hash(n.Text) }

// Hash returns the hex SHA-256 of a text.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Change is one change that the fake front took or refused.
type Change struct {
	Method, Name, Principal, IfHash string
	Text, By, Run                   string
	Sources                         []Source
	Status                          int
}

// Run is one run of the archive of the fake front.
type Run struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Input     string    `json:"input"`
	Output    string    `json:"output"`
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Agent     string    `json:"-"`
	Principal string    `json:"-"`
}

// Front is the fake front.
type Front struct {
	*httptest.Server
	Token string

	mu       sync.Mutex
	notes    map[string]*Note
	changes  []Change
	runs     []Run
	queries  []string
	down     bool
	askFirst bool
	pages    map[string]string
	held     []Change
}

// New starts a fake front that wants the static token.
func New(token string) *Front {
	f := &Front{Token: token, notes: map[string]*Note{}, pages: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func key(agent, scope, principal, name string) string {
	return agent + "|" + scope + "|" + principal + "|" + name
}

// Put sets a note as a person or the after-work review sets it.
func (f *Front) Put(n Note) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n.At.IsZero() {
		n.At = time.Now().UTC()
	}
	f.notes[key(n.Agent, n.Scope, n.Principal, n.Name)] = &n
}

// Drop removes a note, as a front that no longer lists it.
func (f *Front) Drop(agent, scope, principal, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.notes, key(agent, scope, principal, name))
}

// Get returns a note, or false.
func (f *Front) Get(agent, scope, principal, name string) (Note, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.notes[key(agent, scope, principal, name)]
	if !ok {
		return Note{}, false
	}
	return *n, true
}

// Changes returns the changes that the fake front got, in order.
func (f *Front) Changes() []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Change(nil), f.changes...)
}

// AddRun adds a run to the archive.
func (f *Front) AddRun(r Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, r)
}

// Queries returns the raw queries of the searches of runs.
func (f *Front) Queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// SetAskFirst turns ask first on or off: a change that the agent makes
// waits for a person. A new note is kept in the state waiting, and a
// change or a removal of a note that exists leaves the note as it is.
// Each such change answers 202 with the change that waits.
func (f *Front) SetAskFirst(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.askFirst = on
}

// Held returns the changes that wait for a person beside a note that
// exists, in order.
func (f *Front) Held() []Change {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Change(nil), f.held...)
}

// PutPage sets the text of a page of the wiki.
func (f *Front) PutPage(path, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[path] = text
}

// SetDown makes the fake front answer 503 to every request, or answer
// again.
func (f *Front) SetDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

type putBody struct {
	Text    string   `json:"text"`
	IfHash  string   `json:"ifHash"`
	Sources []Source `json:"sources"`
	By      string   `json:"by"`
	Run     string   `json:"run"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Front) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.Token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/v1/knowhow/notes" && r.Method == http.MethodGet:
		f.list(w, q.Get("agent"), q.Get("scope"), q.Get("principal"))
	case r.URL.Path == "/v1/knowhow/runs" && r.Method == http.MethodGet:
		f.queries = append(f.queries, r.URL.RawQuery)
		var out []Run
		for _, run := range f.runs {
			if run.Agent == q.Get("agent") && run.Principal == q.Get("principal") &&
				strings.Contains(strings.ToLower(run.Title+" "+run.Input+" "+run.Output), strings.ToLower(q.Get("q"))) {
				out = append(out, run)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": out})
	case strings.HasPrefix(r.URL.Path, "/v1/pages/") && r.Method == http.MethodGet:
		p := strings.TrimPrefix(r.URL.Path, "/v1/pages/")
		text, ok := f.pages[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": p, "hash": Hash(text), "text": text})
	case strings.HasPrefix(r.URL.Path, "/v1/knowhow/notes/"):
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/knowhow/notes/"), "/", 3)
		if len(parts) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.note(w, r, parts[0], parts[1], q.Get("principal"), parts[2])
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *Front) list(w http.ResponseWriter, agent, scope, principal string) {
	type item struct {
		Name        string    `json:"name"`
		Hash        string    `json:"hash"`
		State       string    `json:"state"`
		Description string    `json:"description"`
		By          string    `json:"by"`
		At          time.Time `json:"at"`
		Uses        int       `json:"uses"`
		Sources     int       `json:"sources"`
	}
	out := []item{}
	for _, n := range f.notes {
		if n.Agent != agent || n.Scope != scope || n.Principal != principal {
			continue
		}
		first, _, _ := strings.Cut(n.Text, "\n")
		out = append(out, item{Name: n.Name, Hash: n.Hash(), State: n.State, Description: first, By: n.By, At: n.At, Uses: n.Uses, Sources: len(n.Sources)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"notes": out})
}

func (f *Front) note(w http.ResponseWriter, r *http.Request, agent, scope, principal, name string) {
	k := key(agent, scope, principal, name)
	cur, exists := f.notes[k]
	switch r.Method {
	case http.MethodGet:
		if !exists || cur.State == "put-away" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": cur.Name, "text": cur.Text, "hash": cur.Hash(), "state": cur.State, "by": cur.By, "sources": cur.Sources})
	case http.MethodPut:
		var b putBody
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c := Change{Method: "PUT", Name: name, Principal: principal, IfHash: b.IfHash, Text: b.Text, By: b.By, Run: b.Run, Sources: b.Sources}
		// A note that is put away is absent: a new text takes its place.
		live := exists && cur.State != "put-away"
		if (b.IfHash == "absent" && live) || (b.IfHash != "absent" && (!live || cur.Hash() != b.IfHash)) {
			c.Status = http.StatusPreconditionFailed
			f.changes = append(f.changes, c)
			w.WriteHeader(c.Status)
			return
		}
		state := "unchecked"
		if exists && cur.State != "put-away" {
			state = cur.State
		}
		if f.askFirst && !strings.HasPrefix(b.By, "person") {
			c.Status = http.StatusAccepted
			f.changes = append(f.changes, c)
			if live && cur.State != "waiting" {
				f.held = append(f.held, c)
			} else {
				f.notes[k] = &Note{Agent: agent, Scope: scope, Principal: principal, Name: name, Text: b.Text, State: "waiting", By: b.By, Run: b.Run, At: time.Now().UTC(), Sources: b.Sources}
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"note": map[string]any{"name": name}, "waiting": map[string]any{"what": "learned"}})
			return
		}
		f.notes[k] = &Note{Agent: agent, Scope: scope, Principal: principal, Name: name, Text: b.Text, State: state, By: b.By, Run: b.Run, At: time.Now().UTC(), Sources: b.Sources}
		c.Status = http.StatusOK
		f.changes = append(f.changes, c)
		writeJSON(w, http.StatusOK, map[string]any{"name": name, "hash": Hash(b.Text)})
	case http.MethodDelete:
		q := r.URL.Query()
		c := Change{Method: "DELETE", Name: name, Principal: principal, IfHash: q.Get("ifHash"), By: q.Get("by"), Run: q.Get("run")}
		switch {
		case !exists || cur.State == "put-away":
			c.Status = http.StatusNotFound
		case cur.Hash() != c.IfHash:
			c.Status = http.StatusPreconditionFailed
		case f.askFirst:
			c.Status = http.StatusAccepted
			f.held = append(f.held, c)
		default:
			cur.State = "put-away"
			c.Status = http.StatusNoContent
		}
		f.changes = append(f.changes, c)
		w.WriteHeader(c.Status)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

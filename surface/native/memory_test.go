package native

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
)

type noteList struct {
	Scope string            `json:"scope"`
	Notes []fw.MemoryRecord `json:"notes"`
}

type hitList struct {
	Scope string         `json:"scope"`
	Hits  []fw.MemoryHit `json:"hits"`
}

// TestMemoryRoutes checks the notes and the search of the native API in the
// agent scope: the owner of the profile reads and writes, a viewer does
// neither, and the routes check the path, the size, and the hash.
func TestMemoryRoutes(t *testing.T) {
	a := manageAgent(t, newScript(endTurn("ok")).model())
	srv := serve(t, a)
	notes := srv.URL + "/v1/memory/notes"
	alice, bob := []string{"Authorization", asAlice}, []string{"Authorization", asBob}

	resp, b := do(t, "PUT", notes+"/build.md?agent=reviewer", "", alice...)
	var empty fw.MemoryRecord
	decodeJSON(t, resp, b, http.StatusOK, &empty)

	put := func(path, body string, header ...string) (*http.Response, []byte) {
		req, err := http.NewRequest("PUT", notes+"/"+path+"?agent=reviewer", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "text/markdown")
		for i := 0; i+1 < len(header); i += 2 {
			req.Header.Set(header[i], header[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res, out
	}

	resp, b = put("build.md", "How to build\nRun make build.\n", alice...)
	var rec fw.MemoryRecord
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	if rec.Ref != "mem://agent/reviewer/build.md" || rec.Title != "How to build" || rec.Hash == "" || resp.Header.Get("ETag") != strconv.Quote(rec.Hash) {
		t.Fatalf("written note = %+v, etag %q", rec, resp.Header.Get("ETag"))
	}

	// A viewer cannot read or write the memory of the agent.
	resp, b = do(t, "GET", notes+"?agent=reviewer", "", bob...)
	decodeProblem(t, resp, b, http.StatusForbidden)
	resp, b = put("bob.md", "x", bob...)
	decodeProblem(t, resp, b, http.StatusForbidden)

	var list noteList
	resp, b = do(t, "GET", notes+"?agent=reviewer", "", alice...)
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if list.Scope != "agent" || len(list.Notes) != 1 || list.Notes[0].Body != "" || list.Notes[0].Title != "How to build" {
		t.Fatalf("list = %+v", list)
	}

	var got fw.MemoryRecord
	resp, b = do(t, "GET", notes+"/build.md?agent=reviewer", "", alice...)
	decodeJSON(t, resp, b, http.StatusOK, &got)
	if got.Body != "How to build\nRun make build.\n" || got.Hash != rec.Hash {
		t.Fatalf("note = %+v", got)
	}
	resp, b = do(t, "GET", notes+"/build.md?agent=reviewer", "", "Authorization", asAlice, "Accept", "text/markdown")
	if resp.StatusCode != http.StatusOK || string(b) != got.Body || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown answer = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}

	// The hash makes a write a compare-and-swap.
	resp, b = put("build.md", "stale\n", "Authorization", asAlice, "If-Match", `"nope"`)
	decodeProblem(t, resp, b, http.StatusPreconditionFailed)
	resp, b = put("build.md", "fresh\n", "Authorization", asAlice, "If-None-Match", "*")
	decodeProblem(t, resp, b, http.StatusPreconditionFailed)
	resp, b = put("build.md", "How to build\nRun make.\n", "Authorization", asAlice, "If-Match", strconv.Quote(rec.Hash))
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	resp, b = put("new.md", "a new note\n", "Authorization", asAlice, "If-None-Match", "*")
	decodeJSON(t, resp, b, http.StatusOK, &got)
	// If-Match: * writes only a note that exists, and a weak tag is a hash.
	resp, b = put("missing.md", "x", "Authorization", asAlice, "If-Match", "*")
	decodeProblem(t, resp, b, http.StatusPreconditionFailed)
	resp, b = put("new.md", "a newer note\n", "Authorization", asAlice, "If-Match", "*")
	decodeJSON(t, resp, b, http.StatusOK, &got)
	resp, b = put("new.md", "the newest note\n", "Authorization", asAlice, "If-Match", `W/"`+got.Hash+`"`)
	decodeJSON(t, resp, b, http.StatusOK, &got)
	// A directory is not a note: DELETE refuses it.
	resp, b = put("dir/inner.md", "x", alice...)
	decodeJSON(t, resp, b, http.StatusOK, &got)
	resp, b = do(t, "DELETE", notes+"/dir?agent=reviewer", "", alice...)
	decodeProblem(t, resp, b, http.StatusBadRequest)

	// The search finds the note by its text.
	var hits hitList
	resp, b = do(t, "GET", srv.URL+"/v1/memory?agent=reviewer&q=make", "", alice...)
	decodeJSON(t, resp, b, http.StatusOK, &hits)
	if len(hits.Hits) == 0 || hits.Hits[0].Record.Ref != "mem://agent/reviewer/build.md" {
		t.Fatalf("hits = %+v", hits)
	}
	resp, b = do(t, "GET", srv.URL+"/v1/memory?agent=reviewer&q=make&kinds=file", "", alice...)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "GET", srv.URL+"/v1/memory?agent=reviewer", "", alice...)
	decodeProblem(t, resp, b, http.StatusBadRequest)

	// The routes check the path, the agent, and the size.
	resp, b = put(".hidden.md", "x", alice...)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "GET", notes+"?agent=nobody", "", alice...)
	decodeProblem(t, resp, b, http.StatusNotFound)
	resp, b = put("big.md", strings.Repeat("x", memory.MaxNoteBytes+1), alice...)
	decodeProblem(t, resp, b, http.StatusRequestEntityTooLarge)

	resp, b = do(t, "DELETE", notes+"/build.md?agent=reviewer", "", alice...)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", notes+"/build.md?agent=reviewer", "", alice...)
	decodeProblem(t, resp, b, http.StatusNotFound)
	resp, b = do(t, "DELETE", notes+"/build.md?agent=reviewer", "", alice...)
	decodeProblem(t, resp, b, http.StatusNotFound)
}

// TestMemoryRoutesPrincipalScope checks that two operators of one profile
// keep separate notes in the principal scope, and that an operator cannot
// write the notes of the agent scope, which every person reads.
func TestMemoryRoutesPrincipalScope(t *testing.T) {
	for _, scope := range []string{"principal", "agent"} {
		t.Run(scope, func(t *testing.T) {
			cfg := fw.DefaultConfig()
			cfg.Home = t.TempDir()
			cfg.Tools.Memory.Scope = scope
			a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithConfig(cfg), fw.WithAuthn(people))
			srv := serve(t, a)
			notes := srv.URL + "/v1/memory/notes"
			resp, b := do(t, "PUT", notes+"/pref.md", "", "Authorization", asAlice)
			if scope == "agent" {
				// An operator of an open profile runs it but does not edit it.
				decodeProblem(t, resp, b, http.StatusForbidden)
				resp, b = do(t, "GET", notes, "", "Authorization", asBob)
				var list noteList
				decodeJSON(t, resp, b, http.StatusOK, &list)
				return
			}
			var rec fw.MemoryRecord
			decodeJSON(t, resp, b, http.StatusOK, &rec)
			if rec.Ref != "mem://principal/default/pref.md" {
				t.Fatalf("ref = %q", rec.Ref)
			}
			var list noteList
			resp, b = do(t, "GET", notes, "", "Authorization", asBob)
			decodeJSON(t, resp, b, http.StatusOK, &list)
			if list.Scope != "principal" || len(list.Notes) != 0 {
				t.Fatalf("bob sees %+v", list)
			}
			resp, b = do(t, "GET", notes, "", "Authorization", asAlice)
			decodeJSON(t, resp, b, http.StatusOK, &list)
			if len(list.Notes) != 1 {
				t.Fatalf("alice sees %+v", list)
			}
			resp, b = do(t, "GET", notes+"/pref.md", "", "Authorization", asBob)
			decodeProblem(t, resp, b, http.StatusNotFound)
		})
	}
}

// syncBuffer is a log sink that several goroutines write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestMemoryRoutesSharedScopeNeedsAdmin checks that a note of the shared
// scope, which every profile of the process reads, is written and removed
// by an admin of the process only, and that each write and removal goes to
// the log with the caller, the op, the size, and the hash, never the text.
func TestMemoryRoutesSharedScopeNeedsAdmin(t *testing.T) {
	cfg := fw.DefaultConfig()
	cfg.Home = t.TempDir()
	cfg.Tools.Memory.Scope = "shared"
	cfg.Access.Admins = []string{"alice"}
	// Bob owns the profile, so he edits it, but he is no admin.
	cfg.Agents = map[string]fw.AgentProfile{"default": {Access: &fw.Access{Owners: []string{"bob"}}}}
	logs := &syncBuffer{}
	a := newAgent(t, newScript(endTurn("ok")).model(), fw.WithConfig(cfg), fw.WithAuthn(people),
		fw.WithLogger(slog.New(slog.NewJSONHandler(logs, nil))))
	srv := serve(t, a)
	notes := srv.URL + "/v1/memory/notes"
	put := func(path, body, auth string) (*http.Response, []byte) {
		req, err := http.NewRequest("PUT", notes+"/"+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", auth)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		out, _ := io.ReadAll(res.Body)
		return res, out
	}

	resp, b := put("team.md", "the secret plan of bob", asBob)
	p := decodeProblem(t, resp, b, http.StatusForbidden)
	if !strings.Contains(p.Detail, "access.admins") {
		t.Fatalf("refusal = %q", p.Detail)
	}
	resp, b = put("team.md", "the team decision\n", asAlice)
	var rec fw.MemoryRecord
	decodeJSON(t, resp, b, http.StatusOK, &rec)
	if rec.Ref != "mem://shared/team.md" {
		t.Fatalf("ref = %q", rec.Ref)
	}
	resp, b = do(t, "DELETE", notes+"/team.md", "", "Authorization", asBob)
	decodeProblem(t, resp, b, http.StatusForbidden)
	// Whoever runs the profile reads the shared notes.
	var list noteList
	resp, b = do(t, "GET", notes, "", "Authorization", asBob)
	decodeJSON(t, resp, b, http.StatusOK, &list)
	if list.Scope != "shared" || len(list.Notes) != 1 {
		t.Fatalf("bob reads %+v", list)
	}
	resp, b = do(t, "DELETE", notes+"/team.md", "", "Authorization", asAlice)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, b)
	}

	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && strings.HasPrefix(fmt.Sprint(m["msg"]), "native: memory note") {
			lines = append(lines, m)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("memory log lines = %v", lines)
	}
	for i, op := range []string{fw.FileCreated, fw.FileRemoved} {
		l := lines[i]
		if l["caller"] != "alice" || l["op"] != op || l["ref"] != "mem://shared/team.md" || l["scope"] != "shared" ||
			l["size"] != float64(len("the team decision\n")) || l["sha256"] != rec.Hash {
			t.Errorf("log line %d = %v", i, l)
		}
	}
	if strings.Contains(logs.String(), "decision") || strings.Contains(logs.String(), "secret plan") {
		t.Fatalf("the log holds the text of a note:\n%s", logs.String())
	}
}

// TestMemoryRoutesHideAgents checks that the routes answer an agent that
// the caller cannot view as they answer an agent that does not exist, as
// GET /v1/agents/{name} does, and that a directory is a bad request.
func TestMemoryRoutesHideAgents(t *testing.T) {
	a := manageAgent(t, newScript(endTurn("ok")).model())
	srv := serve(t, a)
	notes := srv.URL + "/v1/memory/notes"
	detail := func(method, url, auth string, status int) string {
		t.Helper()
		resp, b := do(t, method, url, "", "Authorization", auth)
		return decodeProblem(t, resp, b, status).Detail
	}
	for _, url := range []string{notes + "?agent=%s", notes + "/a.md?agent=%s", srv.URL + "/v1/memory?q=x&agent=%s"} {
		hidden := detail("GET", fmt.Sprintf(url, "private"), asBob, http.StatusNotFound)
		missing := detail("GET", fmt.Sprintf(url, "nobody"), asBob, http.StatusNotFound)
		if strings.Replace(hidden, "private", "nobody", 1) != missing {
			t.Errorf("%s: hidden %q, missing %q", url, hidden, missing)
		}
	}
	if d := detail("PUT", notes+"/a.md?agent=private", asBob, http.StatusNotFound); !strings.Contains(d, `"private"`) {
		t.Errorf("a write to a hidden agent = %q", d)
	}
	if got, want := detail("GET", srv.URL+"/v1/agents/private", asBob, http.StatusNotFound), detail("GET", notes+"?agent=private", asBob, http.StatusNotFound); got != want {
		t.Errorf("/v1/agents says %q, /v1/memory says %q", got, want)
	}

	// A directory is not a note: a bad request, never a server error.
	resp, b := do(t, "PUT", notes+"/dir/inner.md?agent=reviewer", "x", "Authorization", asAlice)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put = %d: %s", resp.StatusCode, b)
	}
	resp, b = do(t, "GET", notes+"/dir?agent=reviewer", "", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)
	resp, b = do(t, "PUT", notes+"/dir?agent=reviewer", "x", "Authorization", asAlice)
	decodeProblem(t, resp, b, http.StatusBadRequest)
}

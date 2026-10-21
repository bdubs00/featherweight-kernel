package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOC is a fake opencode serve: the routes of the HTTP API that the
// driver uses, and an event stream that a script fills for each prompt.
type fakeOC struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	subs     map[chan []byte]string // the event streams, to their directory
	sessions map[string]string      // the sessions, to their directory
	prompts  []fakePrompt
	replies  []fakeReply
	waiters  map[string]chan fakeReply
	aborts   []string
	noAuth   bool // a request came without basic authentication
	calls    int
	oldRoute bool // the server answers 404 on /permission/{id}/reply
	failNext int  // the server answers 500 on this many prompts
	sick     bool // the health route answers 503
	probes   int  // the requests to the health route
	script   func(f *fakeOC, p fakePrompt)
	started  chan fakePrompt
}

type fakePrompt struct {
	Session, Dir, Text, Agent, System string
	Model                             map[string]string
}

type fakeReply struct {
	ID, Reply, Message, Route string
	lost                      bool
}

func newFakeOC(t *testing.T, script func(f *fakeOC, p fakePrompt)) *fakeOC {
	f := &fakeOC{t: t, subs: map[chan []byte]string{}, sessions: map[string]string{}, waiters: map[string]chan fakeReply{},
		script: script, started: make(chan fakePrompt, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /global/health", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		f.mu.Lock()
		f.probes++
		sick := f.sick
		f.mu.Unlock()
		if sick {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"healthy":true,"version":"1.19.3"}`)
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		f.mu.Lock()
		id := "ses_" + strconv.Itoa(len(f.sessions)+1)
		f.sessions[id] = r.URL.Query().Get("directory")
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("POST /session/{id}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		var body struct {
			Model  map[string]string `json:"model"`
			Agent  string            `json:"agent"`
			System string            `json:"system"`
			Parts  []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		p := fakePrompt{Session: r.PathValue("id"), Dir: r.URL.Query().Get("directory"), Agent: body.Agent, System: body.System, Model: body.Model}
		if len(body.Parts) > 0 {
			p.Text = body.Parts[0].Text
		}
		f.mu.Lock()
		if f.failNext > 0 {
			f.failNext--
			f.mu.Unlock()
			http.Error(w, `{"name":"UnknownError"}`, http.StatusInternalServerError)
			return
		}
		f.prompts = append(f.prompts, p)
		f.mu.Unlock()
		f.started <- p
		go f.script(f, p)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		ch := make(chan []byte, 256)
		f.mu.Lock()
		f.subs[ch] = r.URL.Query().Get("directory")
		f.mu.Unlock()
		fmt.Fprint(w, "data: {\"type\":\"server.connected\",\"properties\":{}}\n\n")
		fl.Flush()
		defer func() {
			f.mu.Lock()
			delete(f.subs, ch)
			f.mu.Unlock()
		}()
		for {
			select {
			case b, ok := <-ch:
				if !ok {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /permission/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		if f.oldRoute {
			http.NotFound(w, r)
			return
		}
		var body struct{ Reply, Message string }
		json.NewDecoder(r.Body).Decode(&body)
		if !f.answer(fakeReply{ID: r.PathValue("id"), Reply: body.Reply, Message: body.Message, Route: "permission"}) {
			http.Error(w, `{"name":"PermissionNotFoundError"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "true")
	})
	mux.HandleFunc("POST /session/{sid}/permissions/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		var body struct{ Response string }
		json.NewDecoder(r.Body).Decode(&body)
		if !f.answer(fakeReply{ID: r.PathValue("id"), Reply: body.Response, Route: "session"}) {
			http.Error(w, `{"name":"PermissionNotFoundError"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "true")
	})
	mux.HandleFunc("POST /session/{id}/abort", func(w http.ResponseWriter, r *http.Request) {
		f.auth(r)
		f.mu.Lock()
		f.aborts = append(f.aborts, r.PathValue("id"))
		f.mu.Unlock()
		fmt.Fprint(w, "true")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.close)
	return f
}

func (f *fakeOC) auth(r *http.Request) {
	if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || len(p) < 32 {
		f.mu.Lock()
		f.noAuth = true
		f.mu.Unlock()
	}
}

func (f *fakeOC) close() {
	f.restart()
	f.srv.Close()
}

// restart forgets what an opencode serve keeps in memory: the event
// streams and the permissions that wait. The sessions stay, as in its
// database.
func (f *fakeOC) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		close(ch)
		delete(f.subs, ch)
	}
	for id, ch := range f.waiters {
		ch <- fakeReply{ID: id, lost: true}
		delete(f.waiters, id)
	}
}

func (f *fakeOC) port() int {
	_, p, _ := strings.Cut(strings.TrimPrefix(f.srv.URL, "http://"), ":")
	n, _ := strconv.Atoi(p)
	return n
}

func (f *fakeOC) answer(r fakeReply) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.waiters[r.ID]
	if !ok {
		return false
	}
	delete(f.waiters, r.ID)
	f.replies = append(f.replies, r)
	ch <- r
	return true
}

// emit sends one event to every stream.
func (f *fakeOC) emit(typ string, props map[string]any) {
	b, _ := json.Marshal(map[string]any{"type": typ, "properties": props})
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.subs {
		ch <- b
	}
}

func (f *fakeOC) assistant(sid, mid string) {
	f.emit("message.updated", map[string]any{"sessionID": sid, "info": map[string]any{"id": mid, "sessionID": sid, "role": "assistant"}})
}

func (f *fakeOC) part(sid string, part map[string]any) {
	part["sessionID"] = sid
	f.emit("message.part.updated", map[string]any{"sessionID": sid, "part": part})
}

func (f *fakeOC) status(sid, typ string) {
	f.emit("session.status", map[string]any{"sessionID": sid, "status": map[string]string{"type": typ}})
	if typ == "idle" {
		f.emit("session.idle", map[string]any{"sessionID": sid})
	}
}

// ask asks for a permission and waits for its reply.
func (f *fakeOC) ask(sid, permission, mid, callID string) fakeReply {
	f.mu.Lock()
	f.calls++
	id := "per_" + strconv.Itoa(f.calls)
	ch := make(chan fakeReply, 1)
	f.waiters[id] = ch
	f.mu.Unlock()
	f.emit("permission.asked", map[string]any{"id": id, "sessionID": sid, "permission": permission, "patterns": []string{"*"},
		"metadata": map[string]any{}, "always": []string{"*"}, "tool": map[string]string{"messageID": mid, "callID": callID}})
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		return fakeReply{lost: true}
	}
}

func (f *fakeOC) nextCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return "call_" + strconv.Itoa(f.calls)
}

func tokens(in, out int) map[string]any {
	return map[string]any{"input": in, "output": out, "reasoning": 0, "cache": map[string]int{"read": 4, "write": 0}}
}

// writeHello is the script of a session that writes hello.txt with the
// write tool, which asks for the permission edit, and then answers.
func writeHello(f *fakeOC, p fakePrompt) {
	sid := p.Session
	f.status(sid, "busy")
	call := f.nextCall()
	m1, m2 := "msg_a"+call, "msg_b"+call
	f.assistant(sid, m1)
	f.part(sid, map[string]any{"id": "prt_s1", "messageID": m1, "type": "step-start"})
	f.part(sid, map[string]any{"id": "prt_t1", "messageID": m1, "type": "text", "text": "I will write the file."})
	input := map[string]any{"filePath": "hello.txt", "content": "hello"}
	tool := map[string]any{"id": "prt_" + call, "messageID": m1, "type": "tool", "callID": call, "tool": "write",
		"state": map[string]any{"status": "running", "input": input, "time": map[string]int{"start": 1}}}
	f.part(sid, tool)
	rep := f.ask(sid, "edit", m1, call)
	if rep.lost {
		return
	}
	if rep.Reply == "once" {
		os.WriteFile(filepath.Join(p.Dir, "hello.txt"), []byte("hello"), 0o644)
		tool["state"] = map[string]any{"status": "completed", "input": input, "output": "Wrote hello.txt", "title": "hello.txt", "metadata": map[string]any{}}
	} else {
		tool["state"] = map[string]any{"status": "error", "input": input, "error": "rejected: " + rep.Message}
	}
	f.part(sid, tool)
	f.part(sid, map[string]any{"id": "prt_f1", "messageID": m1, "type": "step-finish", "reason": "tool-calls", "cost": 0.001, "tokens": tokens(100, 20)})
	f.assistant(sid, m2)
	f.part(sid, map[string]any{"id": "prt_s2", "messageID": m2, "type": "step-start"})
	f.part(sid, map[string]any{"id": "prt_t2", "messageID": m2, "type": "text", "text": "Done."})
	f.part(sid, map[string]any{"id": "prt_f2", "messageID": m2, "type": "step-finish", "reason": "stop", "cost": 0.001, "tokens": tokens(130, 5)})
	f.status(sid, "idle")
}

// answerOnly is the script of a session that answers with text.
func answerOnly(f *fakeOC, p fakePrompt) {
	sid := p.Session
	f.status(sid, "busy")
	m := "msg_" + f.nextCall()
	f.assistant(sid, m)
	f.part(sid, map[string]any{"id": "prt_s", "messageID": m, "type": "step-start"})
	f.part(sid, map[string]any{"id": "prt_t", "messageID": m, "type": "text", "text": "Again."})
	f.part(sid, map[string]any{"id": "prt_f", "messageID": m, "type": "step-finish", "reason": "stop", "cost": 0, "tokens": tokens(10, 2)})
	f.status(sid, "idle")
}

// silent is the script of a session that starts to work and then sends
// nothing, not even a heartbeat.
func silent(f *fakeOC, p fakePrompt) {
	f.status(p.Session, "busy")
}

// readAt returns the script of a session that reads the file p with the
// read tool, which asks for the permission read.
func readAt(p string) func(f *fakeOC, fp fakePrompt) {
	return func(f *fakeOC, fp fakePrompt) {
		sid := fp.Session
		f.status(sid, "busy")
		call := f.nextCall()
		m := "msg_" + call
		f.assistant(sid, m)
		f.part(sid, map[string]any{"id": "prt_s", "messageID": m, "type": "step-start"})
		input := map[string]any{"filePath": p}
		tool := map[string]any{"id": "prt_" + call, "messageID": m, "type": "tool", "callID": call, "tool": "read",
			"state": map[string]any{"status": "running", "input": input}}
		f.part(sid, tool)
		rep := f.ask(sid, "read", m, call)
		if rep.lost {
			return
		}
		if rep.Reply == "once" {
			tool["state"] = map[string]any{"status": "completed", "input": input, "output": "contents"}
		} else {
			tool["state"] = map[string]any{"status": "error", "input": input, "error": "rejected: " + rep.Message}
		}
		f.part(sid, tool)
		f.part(sid, map[string]any{"id": "prt_f", "messageID": m, "type": "step-finish", "reason": "stop", "cost": 0, "tokens": tokens(10, 2)})
		f.status(sid, "idle")
	}
}

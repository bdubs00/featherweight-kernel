//go:build unix

package env

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

const routeToken = "front-static-token"

// serveRoutes gives the agent of f the native surface with the routes of
// the environment behind a static token, and returns its server.
func serveRoutes(t *testing.T, f *fixture) *httptest.Server {
	t.Helper()
	f.extra = append(f.extra, fw.WithAuthn(fw.TokenAuthn{Token: routeToken}), fw.WithSurfaces(native.NewWith(mount)))
	h, err := f.agent().Handler()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, srv *httptest.Server, method, body string, token bool) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+"/v1/environment", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token {
		req.Header.Set("Authorization", "Bearer "+routeToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func viewOf(t *testing.T, b []byte) View {
	t.Helper()
	var v View
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("view %s: %v", b, err)
	}
	return v
}

// waitState waits until the state of the block is kind with the digest of
// the block, and returns it.
func waitState(t *testing.T, f *fixture, kind string) state {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s, err := load(f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		f.s = s
		st := s.state()
		if st.Kind == kind {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want %s", st.Kind, kind)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The front pushes the packages of a running worker: the push replaces
// the lists and the switch of the block as the base, drops the local
// changes, and the service installs it in the background. The events
// carry the switch and the lists.
func TestPushRoutes(t *testing.T) {
	f := newFixture(t, "packages:\n  python: [pyyaml]\n  tools: []\n")
	srv := serveRoutes(t, f)
	if code, _ := call(t, srv, "GET", "", false); code != http.StatusUnauthorized {
		t.Fatalf("GET without a token = %d", code)
	}
	code, b := call(t, srv, "GET", "", true)
	v := viewOf(t, b)
	if code != http.StatusOK || !v.On || !v.Ask || !slices.Equal(v.Lists.Python, []string{"pyyaml"}) || len(v.Lists.Tools) != 0 || v.State != StatePending || !strings.HasPrefix(v.Digest, "sha256:") {
		t.Fatalf("GET = %d %s", code, b)
	}
	c, err := Start(context.Background(), f.agent())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitState(t, f, StateReady)

	// A local change of the tool env, which the next base drops.
	tl, _ := newTool(f.cfg)
	if res := callTool(t, f.runCtx(), tl, `{"command":"add","manager":"python","package":"requests"}`); res.IsError {
		t.Fatalf("add = %+v", res)
	}
	if got := f.s.effective(pkglist.Python); !slices.Equal(got, []string{"pyyaml", "requests"}) {
		t.Fatalf("effective = %v", got)
	}

	for body, want := range map[string]string{
		`{"python": []}`: "the body needs on and ask",
		`{"on": true, "ask": true, "python": ["-e ."]}`: "python item 0 starts with a dash",
		`{"on": true, "ask": true, "tools": ["a\tb"]}`:  "tools item 0 holds a control character",
		`{"on": true, "ask": true, "python": "pyyaml"}`: "",
	} {
		code, b := call(t, srv, "PUT", body, true)
		if code != http.StatusBadRequest || !strings.Contains(string(b), want) {
			t.Errorf("PUT %s = %d %s, want 400 %q", body, code, b, want)
		}
	}
	if _, err := os.Stat(f.s.pushedPath()); err == nil {
		t.Fatal("a bad push left a file")
	}

	code, b = call(t, srv, "PUT", `{"on": true, "ask": false, "python": ["pyyaml", " six "], "tools": ["jq"]}`, true)
	v = viewOf(t, b)
	if code != http.StatusAccepted || !v.On || v.Ask || !slices.Equal(v.Lists.Python, []string{"pyyaml", "six"}) || !slices.Equal(v.Lists.Tools, []string{"jq"}) {
		t.Fatalf("PUT = %d %s", code, b)
	}
	info, err := os.Stat(f.s.pushedPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the push file: %v, %v", info, err)
	}
	st := waitState(t, f, StateReady)
	if st.Marker.Digest != f.s.digest() || st.Marker.Tools == nil || !f.s.pushed {
		t.Fatalf("after the push: %+v", st.Marker)
	}
	if f.s.readLocal() != nil {
		t.Fatal("the push kept the local changes")
	}
	// The events carry the switch and the effective lists.
	deadline := time.Now().Add(5 * time.Second)
	var ready ReadyData
	for time.Now().Before(deadline) {
		for _, e := range f.settled() {
			if e.Type != EvReady {
				continue
			}
			var r ReadyData
			_ = json.Unmarshal(e.Data, &r)
			if r.Marker != nil && r.Digest == st.Marker.Digest {
				ready = r
			}
		}
		if ready.Marker != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ready.Marker == nil || !ready.On || ready.Ask || ready.Lists == nil || !slices.Equal(ready.Lists.Python, []string{"pyyaml", "six"}) || !slices.Equal(ready.Lists.Tools, []string{"jq"}) {
		t.Fatalf("ready = %+v", ready)
	}

	// With ask false, add and remove refuse, and list works.
	tl, _ = newTool(f.cfg)
	if res := callTool(t, f.runCtx(), tl, `{"command":"add","manager":"python","package":"requests"}`); !res.IsError || fw.TextOf(res.Content) != MsgNoAsk {
		t.Fatalf("add with ask false = %+v", res)
	}
	if res := callTool(t, f.runCtx(), tl, `{"command":"list"}`); res.IsError || !strings.Contains(fw.TextOf(res.Content), "cannot ask for packages") {
		t.Fatalf("list = %+v", res)
	}
	if text := section(f.agent(), fw.AgentProfile{}); !strings.Contains(text, "cannot change its packages") {
		t.Fatalf("section with ask false:\n%s", text)
	}

	// With on false, the lists are empty, the environment goes, the
	// section leaves the prompt, and add refuses.
	code, b = call(t, srv, "PUT", `{"on": false, "ask": true, "python": ["pyyaml"], "tools": ["jq"]}`, true)
	v = viewOf(t, b)
	if code != http.StatusAccepted || v.On || len(v.Lists.Python) != 0 || len(v.Lists.Tools) != 0 {
		t.Fatalf("PUT off = %d %s", code, b)
	}
	st = waitState(t, f, StateReady)
	if st.Marker.Python != nil || st.Marker.Tools != nil || isDir(f.s.venv()) {
		t.Fatalf("the environment stayed with on false: %+v", st.Marker)
	}
	if text := section(f.agent(), fw.AgentProfile{}); text != "" {
		t.Fatalf("section with on false:\n%s", text)
	}
	if res := callTool(t, f.runCtx(), tl, `{"command":"remove","manager":"python","package":"pyyaml"}`); !res.IsError || fw.TextOf(res.Content) != MsgOff {
		t.Fatalf("remove with on false = %+v", res)
	}
}

// Without the block, both routes answer 404. A block that names a
// manifest refuses a push of lists with 409.
func TestPushRoutesRefuse(t *testing.T) {
	none := newFixture(t, "")
	srv := serveRoutes(t, none)
	for _, m := range []string{"GET", "PUT"} {
		if code, b := call(t, srv, m, `{"on": true, "ask": true}`, true); code != http.StatusNotFound || !strings.Contains(string(b), "no block environment") {
			t.Fatalf("%s without the block = %d %s", m, code, b)
		}
	}
	files := newFixture(t, fullBlock)
	srv = serveRoutes(t, files)
	if code, b := call(t, srv, "PUT", `{"on": true, "ask": true, "python": ["six"]}`, true); code != http.StatusConflict || !strings.Contains(string(b), "names a manifest") {
		t.Fatalf("PUT on a manifest = %d %s", code, b)
	}
}

// A package that does not resolve fails the install, which then waits for
// the longest pause. A tool process keeps the last environment that
// installed, and the section names the failure in one sentence. A push
// that fixes the list installs at once.
func TestFailedInstallKeepsTheLastEnvironment(t *testing.T) {
	f := newFixture(t, "packages:\n  python: [pyyaml]\n")
	if err := f.install(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.writePushed(&Pushed{On: true, Ask: true, Python: []string{"pyyaml", "nosuch-fw-walk"}}); err != nil {
		t.Fatal(err)
	}
	err := f.install()
	if err == nil || !strings.Contains(err.Error(), "uv lock") {
		t.Fatalf("err = %v", err)
	}
	fl := f.s.readFailure()
	if fl == nil || fl.RetryAt.Sub(fl.Time) != backoffMax {
		t.Fatalf("an unsolvable list waits %v", fl.RetryAt.Sub(fl.Time))
	}
	env, err := fw.ExtendToolEnv(f.runCtx(), f.cfg, []string{"PATH=/bin"})
	if err != nil || envValue(env, "VIRTUAL_ENV") != f.s.venv() || !strings.HasPrefix(envValue(env, "PATH"), f.s.venv()) {
		t.Fatalf("a tool process after the failure: %v %v", err, env)
	}
	text := section(f.agent(), fw.AgentProfile{})
	if !strings.Contains(text, "The last change of the packages failed to install at the step python, so the shell has the packages of the last install that worked.") || !strings.Contains(text, "pyyaml") {
		t.Fatalf("section:\n%s", text)
	}
	// The service waits for the longest pause, but a fixed list installs
	// at once.
	c, err := Start(context.Background(), f.agent())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond)
	if err := f.s.writePushed(&Pushed{On: true, Ask: true, Python: []string{"pyyaml", "six"}}); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, f, StateReady)
	if !slices.ContainsFunc(st.Marker.Python.Packages, func(p Package) bool { return p.Name == "six" }) {
		t.Fatalf("marker = %+v", st.Marker.Python)
	}
}

// A network failure is not an answer of the resolver, so it keeps the
// short pause.
func TestUnsolvable(t *testing.T) {
	for out, want := range map[string]bool{
		"error: No solution found when resolving dependencies\n  cause: Because x was not found in the package registry": true,
		"Error: failed to solve requirements of environment 'default'\n  × failed to solve the environment":              true,
		"Cannot solve the request because of: No candidates were found for x":                                            true,
		"cause: error sending request for url (http://127.0.0.1:1/simple/x/)\n  cause: tcp connect error":                false,
		"repodata_shards.msgpack.zst)\n├─▶ client error (Connect)":                                                       false,
	} {
		if got := unsolvable(&stepError{step: "python", err: bytes.ErrTooLarge, output: out}); got != want {
			t.Errorf("unsolvable(%q) = %v", out, got)
		}
	}
}

// The block takes the switch too, so a new Task boots with the setting of
// its pool before the first push.
func TestSwitchOfTheBlock(t *testing.T) {
	off := newFixture(t, "on: false\nask: false\npackages:\n  python: [six]\n  tools: [jq]\n")
	if off.s.on || off.s.ask || len(off.s.effective(pkglist.Python)) != 0 || len(off.s.effective(pkglist.Tools)) != 0 {
		t.Fatalf("spec = %+v", off.s)
	}
	on := newFixture(t, "packages:\n  python: [six]\n")
	if !on.s.on || !on.s.ask || on.s.pushed {
		t.Fatalf("spec = %+v", on.s)
	}
	if on.s.listsDigest() == off.s.listsDigest() {
		t.Fatal("the switch is not part of the base")
	}
}

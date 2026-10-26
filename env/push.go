package env

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/surface/native"
)

// The front changes the packages of a running worker in place, through
// PUT /v1/environment, because a new Task starts with an empty volume and
// loses the runs of the agent. The kernel keeps the body in the file
// .fw-env-pushed.json under dir. It replaces the lists and the switch of
// the block as the base (spec.base). A change of the base drops the local
// changes of the tool env, and the service installs the new base in the
// background.

// pushedFile is the name of the push of the front under dir.
const pushedFile = ".fw-env-pushed.json"

// Pushed is the file .fw-env-pushed.json: the setting that the front
// pushed with PUT /v1/environment.
type Pushed struct {
	On     bool      `json:"on"`
	Ask    bool      `json:"ask"`
	Python []string  `json:"python"`
	Tools  []string  `json:"tools"`
	At     time.Time `json:"at"`
	By     string    `json:"by,omitempty"`
}

func (s *spec) pushedPath() string { return filepath.Join(s.dir, pushedFile) }

// readPushed reads the push of the front. A file that fw does not own
// while a tool user exists, or a list that does not fit, counts as no
// push.
func (s *spec) readPushed() *Pushed {
	var p Pushed
	if !s.readOwned(s.pushedPath(), &p) {
		return nil
	}
	if pkglist.Check(pkglist.Python, p.Python) != nil || pkglist.Check(pkglist.Tools, p.Tools) != nil {
		return nil
	}
	p.Python, p.Tools = trimList(p.Python), trimList(p.Tools)
	return &p
}

// writePushed writes the push of the front with the mode 0600.
func (s *spec) writePushed(p *Pushed) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return writeFileMode(s.pushedPath(), p, 0o600)
}

func trimList(list []string) []string {
	out := make([]string, 0, len(list))
	for _, req := range list {
		out = append(out, strings.TrimSpace(req))
	}
	return out
}

// ListsData names the effective lists: the base with the local changes.
type ListsData struct {
	Python []string `json:"python"`
	Tools  []string `json:"tools"`
}

// Switch is the switch of the packages and the effective lists. The
// events fw.env.ready and fw.env.failed carry it, and so does the answer
// of /v1/environment.
type Switch struct {
	On    bool       `json:"on"`
	Ask   bool       `json:"ask"`
	Lists *ListsData `json:"lists,omitempty"`
}

// ReadyData is the data of fw.env.ready: the marker and the switch.
type ReadyData struct {
	*Marker
	Switch
}

// FailedData is the data of fw.env.failed: the failure and the switch.
type FailedData struct {
	*Failure
	Switch
}

// switchOf returns the switch and the effective lists of s. A manager
// with a manifest has no list.
func (s *spec) switchOf() Switch {
	sw := Switch{On: s.on, Ask: s.ask}
	if s.inline != nil {
		sw.Lists = &ListsData{Python: s.effective(pkglist.Python), Tools: s.effective(pkglist.Tools)}
		if sw.Lists.Python == nil {
			sw.Lists.Python = []string{}
		}
		if sw.Lists.Tools == nil {
			sw.Lists.Tools = []string{}
		}
	}
	return sw
}

// View is the answer of GET and PUT /v1/environment.
type View struct {
	On      bool      `json:"on"`
	Ask     bool      `json:"ask"`
	Lists   ListsData `json:"lists"`
	State   string    `json:"state"`
	Digest  string    `json:"digest"`
	Marker  *Marker   `json:"marker,omitempty"`
	Failure *Failure  `json:"failure,omitempty"`
}

func (s *spec) view() View {
	st := s.state()
	sw := s.switchOf()
	v := View{On: sw.On, Ask: sw.Ask, Lists: ListsData{Python: []string{}, Tools: []string{}}, State: st.Kind, Digest: st.Digest, Marker: st.Marker, Failure: st.Failure}
	if sw.Lists != nil {
		v.Lists = *sw.Lists
	}
	return v
}

// pushBody is the body of PUT /v1/environment.
type pushBody struct {
	On     *bool    `json:"on"`
	Ask    *bool    `json:"ask"`
	Python []string `json:"python"`
	Tools  []string `json:"tools"`
}

// mount adds GET and PUT /v1/environment to the native surface.
func mount(k *native.Kit) error {
	a := k.Agent()
	k.Handle("GET /v1/environment", func(w http.ResponseWriter, r *http.Request) {
		s, ok := routeSpec(w, r, a)
		if !ok {
			return
		}
		if err := authorize(r.Context(), a, fw.ActionAgentView); err != nil {
			k.Fail(w, r, err)
			return
		}
		native.WriteJSON(w, http.StatusOK, s.view())
	})
	k.Handle("PUT /v1/environment", func(w http.ResponseWriter, r *http.Request) {
		s, ok := routeSpec(w, r, a)
		if !ok {
			return
		}
		if err := authorize(r.Context(), a, fw.ActionAgentRun); err != nil {
			k.Fail(w, r, err)
			return
		}
		var body pushBody
		if !k.Decode(w, r, &body) {
			return
		}
		if body.On == nil || body.Ask == nil {
			native.WriteProblem(w, r, http.StatusBadRequest, "the body needs on and ask")
			return
		}
		for _, l := range []struct {
			name string
			list []string
		}{{pkglist.Python, body.Python}, {pkglist.Tools, body.Tools}} {
			if err := pkglist.Check(l.name, l.list); err != nil {
				native.WriteProblem(w, r, http.StatusBadRequest, fmt.Sprintf("%s %v", l.name, err))
				return
			}
		}
		switch {
		case s.placement == PlacementImage:
			native.WriteProblem(w, r, http.StatusConflict, "the environment of this agent is part of its image (placement image), so a push does not apply")
			return
		case s.manifest[pkglist.Python] || s.manifest[pkglist.Tools]:
			native.WriteProblem(w, r, http.StatusConflict, "the block environment of this agent names a manifest, so a push of lists does not apply")
			return
		}
		p := &Pushed{On: *body.On, Ask: *body.Ask, Python: trimList(body.Python), Tools: trimList(body.Tools), At: time.Now().UTC()}
		if pr := fw.PrincipalFrom(r.Context()); pr != nil {
			p.By = pr.Subject
		}
		if err := s.writePushed(p); err != nil {
			native.WriteProblem(w, r, http.StatusInternalServerError, "env: keep the push: "+native.Cause(err))
			return
		}
		s, ok = routeSpec(w, r, a)
		if !ok {
			return
		}
		in := newInstaller(a, a.Config(), s)
		in.log.Info("the front pushed the packages", "on", p.On, "ask", p.Ask, "python", len(p.Python), "tools", len(p.Tools), "by", p.By)
		// The notifier sends the new switch and lists, also when the
		// digest stays and nothing installs. The service installs a new
		// digest in the background.
		in.owe()
		native.WriteJSON(w, http.StatusAccepted, s.view())
	})
	return nil
}

// routeSpec loads the block for a route, or answers 404 without it.
func routeSpec(w http.ResponseWriter, r *http.Request, a *fw.Agent) (*spec, bool) {
	s, err := load(a.Config())
	switch {
	case err != nil:
		native.WriteProblem(w, r, http.StatusInternalServerError, native.Cause(err))
		return nil, false
	case s == nil:
		native.WriteProblem(w, r, http.StatusNotFound, "this agent has no block environment")
		return nil, false
	}
	return s, true
}

// authorize checks the action on every profile of the process, because
// the environment serves them all. A process without a profile needs the
// admin role.
func authorize(ctx context.Context, a *fw.Agent, action string) error {
	profiles, err := a.ListProfiles(context.Background())
	if err != nil {
		return err
	}
	if len(profiles) == 0 {
		return a.Authorize(ctx, fw.ActionAdmin, fw.Target{})
	}
	checks := make([]fw.AuthzCheck, 0, len(profiles))
	for _, p := range profiles {
		checks = append(checks, fw.AuthzCheck{Action: action, Target: fw.Target{Agent: p.Name}})
	}
	for _, err := range a.AuthorizeAll(ctx, checks) {
		if err != nil {
			return err
		}
	}
	return nil
}

// writeFileMode is writeFile with the mode of the file.
func writeFileMode(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

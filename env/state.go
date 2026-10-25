package env

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
)

// States of an environment, as fw doctor and the tool env name them.
const (
	StateNone          = "none"          // the configuration has no block environment
	StatePending       = "pending"       // nothing is installed for the digest yet
	StateMaterializing = "materializing" // a process installs it now
	StateReady         = "ready"         // the marker holds the digest
	StateFailed        = "failed"        // the last install of the digest failed
)

// Package is one installed package. The JSON names are those of
// `uv pip list --format json` and `pixi list --json`, so the Dockerfile
// writes their output into the marker as it is.
type Package struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Explicit bool   `json:"is_explicit,omitempty"`
}

// PythonInfo is the Python part of the marker.
type PythonInfo struct {
	Manifest string    `json:"manifest"`          // relative to the workspace
	Version  string    `json:"version,omitempty"` // of the interpreter
	Env      string    `json:"env,omitempty"`     // the virtual environment
	Packages []Package `json:"packages,omitempty"`
}

// ToolsInfo is the pixi part of the marker.
type ToolsInfo struct {
	Manifest string    `json:"manifest"`         // relative to the workspace
	Prefix   string    `json:"prefix,omitempty"` // the environment that pixi installed
	Packages []Package `json:"packages,omitempty"`
}

// Marker is the file .fw-env.json under dir. It records that the
// environment of Digest is installed, and it is the data of the event
// fw.env.ready.
type Marker struct {
	Digest    string      `json:"digest"`
	Placement string      `json:"placement"`
	Ready     time.Time   `json:"ready"`
	Seconds   float64     `json:"seconds"`
	UV        string      `json:"uv,omitempty"`   // the output of uv --version
	Pixi      string      `json:"pixi,omitempty"` // the output of pixi --version
	Python    *PythonInfo `json:"python,omitempty"`
	Tools     *ToolsInfo  `json:"tools,omitempty"`
	Prepare   int         `json:"prepare,omitempty"` // the prepare commands that ran
}

// Failure is the file .fw-env-failed.json under dir, and the data of the
// event fw.env.failed. Output is the end of the output of the manager,
// with the secrets of the process and the passwords of URLs replaced.
type Failure struct {
	Digest  string    `json:"digest"`
	Time    time.Time `json:"time"`
	Step    string    `json:"step"` // python, tools, prepare 1, or the layout
	Error   string    `json:"error"`
	Output  string    `json:"output,omitempty"`
	Attempt int       `json:"attempt"`
	RetryAt time.Time `json:"retryAt"`
}

// state is what fw doctor, the prompt, and the wait read.
type state struct {
	Kind    string
	Digest  string
	Marker  *Marker  // the marker, also when it holds another digest
	Failure *Failure // the failure of Digest, or nil
}

// digest returns the sha256 of the manifests, the lock files, the
// mirrors, and the prepare commands. A missing file counts as missing, so
// the digest changes when it appears. For a list of environment.packages,
// the digest covers the effective list in sorted order and not the
// manifest and the lock file that the kernel writes. A block without
// lists and mirrors keeps the digest of the files alone, so an
// environment of an older kernel stays ready.
func (s *spec) digest() string {
	h := sha256.New()
	part := func(label string, b []byte) {
		fmt.Fprintf(h, "%s %d\n", label, len(b))
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	file := func(label, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(h, "%s missing\n", label)
			return
		}
		part(label, b)
	}
	if s.inline != nil {
		part("lists", []byte("1"))
	}
	switch {
	case s.isInline(pkglist.Python):
		s.inlineDigest(part, pkglist.Python)
	case s.python != "":
		file("python manifest", s.python)
		file("python lock", s.uvLock())
	}
	switch {
	case s.isInline(pkglist.Tools):
		s.inlineDigest(part, pkglist.Tools)
	case s.tools != "":
		file("tools manifest", s.tools)
		file("tools lock", s.pixiLock())
	}
	for _, m := range []string{pkglist.Python, pkglist.Tools} {
		if u := s.mirrors[m]; u != "" {
			part(m+" mirror", []byte(u))
		}
	}
	for _, p := range s.prepare {
		part("prepare", []byte(p))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// short returns the first 12 hex digits of a digest, for a log line.
func short(digest string) string {
	if len(digest) > len("sha256:")+12 {
		return digest[:len("sha256:")+12]
	}
	return digest
}

// state reads the state of the environment from dir.
func (s *spec) state() state {
	st := state{Digest: s.digest()}
	st.Marker = s.readMarker()
	if s.placement == PlacementImage {
		// The image holds the environment, and nothing installs it here.
		if st.Marker != nil {
			st.Kind = StateReady
		} else {
			st.Kind = StateFailed
			st.Failure = &Failure{Digest: st.Digest, Step: "image", Error: "the image holds no environment at " + s.dir + "; build it with fw env dockerfile"}
		}
		return st
	}
	if m := st.Marker; m != nil && m.Digest == st.Digest && s.intact(m) {
		st.Kind = StateReady
		return st
	}
	if locked(s.lockPath()) {
		st.Kind = StateMaterializing
		return st
	}
	if f := s.readFailure(); f != nil && f.Digest == st.Digest {
		st.Kind, st.Failure = StateFailed, f
		return st
	}
	st.Kind = StatePending
	return st
}

// intact reports whether the directories that a marker names still exist,
// so a marker without its environment counts as not ready. A list without
// a package has no environment, and its part of the marker is nil.
func (s *spec) intact(m *Marker) bool {
	if s.python != "" && (m.Python != nil || !s.isInline(pkglist.Python)) {
		if _, err := os.Stat(filepath.Join(s.venv(), "bin", "python")); err != nil {
			return false
		}
	}
	if s.tools != "" && m.Tools != nil && m.Tools.Prefix != "" {
		if _, err := os.Stat(m.Tools.Prefix); err != nil {
			return false
		}
	}
	return true
}

// readMarker reads the marker. A marker that the user of fw does not own
// counts as absent when a tool user exists, because the tool user could
// have written it.
func (s *spec) readMarker() *Marker {
	var m Marker
	if !s.readOwned(s.markerPath(), &m) || m.Digest == "" {
		return nil
	}
	return &m
}

// readFailure reads the record of the last failure.
func (s *spec) readFailure() *Failure {
	var f Failure
	if !s.readOwned(s.failurePath(), &f) || f.Digest == "" {
		return nil
	}
	return &f
}

func (s *spec) readOwned(path string, v any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if s.user != nil {
		info, err := f.Stat()
		if err != nil || !ownedBySelf(info) {
			return false
		}
	}
	return json.NewDecoder(f).Decode(v) == nil
}

// writeFile writes a state file through a temporary file and a rename, so
// a reader never sees half of it. The temporary file has a random name
// and a new inode. The tool user can add entries to dir, and with a fixed
// name, a link of that user can lead the write of fw elsewhere.
func writeFile(path string, v any) error {
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
		err = f.Chmod(0o644)
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

// removeFile removes a state file that can be absent.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// backoff is the wait before the next install after attempt failures in
// a row: 5 seconds, doubling, at most 5 minutes.
func backoff(attempt int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempt && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

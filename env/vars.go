package env

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
)

// defaultPath is the PATH of a tool process that has none.
const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// pixiPrefix returns the pixi environment: the fixed directory of an
// image, else the prefix that the marker records, else the one directory
// that pixi made under pixi-envs, else "".
func (s *spec) pixiPrefix(m *Marker) string {
	if s.tools == "" {
		return ""
	}
	if s.placement == PlacementImage {
		return s.imagePixiPrefix()
	}
	if m != nil && m.Tools != nil && m.Tools.Prefix != "" {
		return m.Tools.Prefix
	}
	found, _ := filepath.Glob(filepath.Join(s.pixiEnvs(), "*", "envs", "default"))
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// paths returns the directories that go in front of PATH, in order: the
// virtual environment, the pixi environment, the programs of uv tools and
// the links to the Python of uv, and the programs of pixi global.
func (s *spec) paths(m *Marker) []string {
	var out []string
	if s.python != "" {
		out = append(out, filepath.Join(s.venv(), "bin"))
	}
	if p := s.pixiPrefix(m); p != "" {
		out = append(out, filepath.Join(p, "bin"))
	}
	return append(out, s.binDir(), filepath.Join(s.pixiHome(), "bin"))
}

// vars returns the variables of the environment as NAME=value. The
// variables of uv keep its cache, its interpreters, and its tools under
// dir and cache. PIXI_HOME holds the configuration of pixi, which keeps
// each environment under dir (detached-environments), and the programs of
// pixi global. With dir and cache on two file systems, uv copies the files
// of a package instead of linking them. A mirror of Python is the index of
// uv and pip. pip ignores an index on plain http unless it trusts the
// host, so an http mirror also names its host in PIP_TRUSTED_HOST.
func (s *spec) vars(m *Marker) []string {
	out := []string{
		"UV_CACHE_DIR=" + s.uvCache(),
		"UV_PYTHON_INSTALL_DIR=" + s.pythonDir(),
		"UV_PYTHON_BIN_DIR=" + s.binDir(),
		"UV_TOOL_DIR=" + s.uvToolDir(),
		"UV_TOOL_BIN_DIR=" + s.binDir(),
		"PIXI_HOME=" + s.pixiHome(),
		"PIXI_CACHE_DIR=" + s.pixiCache(),
	}
	if s.python != "" && (!s.isInline(pkglist.Python) || isDir(s.venv())) {
		// A list without a package has no virtual environment.
		out = append(out, "UV_PROJECT_ENVIRONMENT="+s.venv(), "VIRTUAL_ENV="+s.venv())
	}
	if m := s.mirrors[pkglist.Python]; m != "" {
		out = append(out, "UV_DEFAULT_INDEX="+m, "PIP_INDEX_URL="+m)
		if u, err := url.Parse(m); err == nil && u.Scheme == "http" {
			out = append(out, "PIP_TRUSTED_HOST="+u.Hostname())
		}
	}
	if p := s.pixiPrefix(m); p != "" {
		out = append(out, "CONDA_PREFIX="+p)
	}
	if !sameDevice(s.dir, s.cache) {
		out = append(out, "UV_LINK_MODE=copy")
	}
	return out
}

// apply returns env with the directories of the environment in front of
// its PATH and the variables of the environment in place of the same
// names.
func (s *spec) apply(env []string, m *Marker) []string {
	add := s.vars(m)
	names := map[string]bool{}
	for _, kv := range add {
		name, _, _ := strings.Cut(kv, "=")
		names[name] = true
	}
	path := ""
	out := make([]string, 0, len(env)+len(add)+1)
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case name == "PATH":
			path = value
		case names[name]:
		default:
			out = append(out, kv)
		}
	}
	if path == "" {
		path = defaultPath
	}
	front := strings.Join(s.paths(m), string(os.PathListSeparator))
	out = append(out, "PATH="+front+string(os.PathListSeparator)+path)
	return append(out, add...)
}

// lookupEnv returns the value of the last entry of name in env, or "".
func lookupEnv(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if n, val, ok := strings.Cut(kv, "="); ok && n == name {
			v = val
		}
	}
	return v
}

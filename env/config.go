// Package env gives an agent a package environment on the durable volume
// (docs/ARCHITECTURE.md section 18). It claims the block environment of
// fw.yaml. The block names a uv project for Python and a pixi manifest for
// other tools, each with its lock file, or it names the packages in plain
// lists, and the package writes the manifests and the lock files itself.
// The package installs them once per digest, in the background, as the
// tool user, and gives every tool process the variables of the
// environment. It adds the tool env, which adds and
// removes packages through the managers, the Environment section of the
// system prompt, a line of fw doctor, and the verb fw env dockerfile.
//
// uv and pixi are single static binaries. The package never downloads
// them: the image must carry them on PATH.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bdubs00/featherweight-kernel/env/pkglist"
	"github.com/bdubs00/featherweight-kernel/fw"
)

// Block is the top-level key of fw.yaml that the package claims.
const Block = "environment"

// Placements of the environment.
const (
	// PlacementDurable installs the environment under dir at run time. Under
	// ax, dir is on the durable volume, so the environment stays after a
	// suspend.
	PlacementDurable = "durable"
	// PlacementImage says that the image holds the environment at dir
	// already, built from the Dockerfile of fw env dockerfile. The package
	// installs nothing then.
	PlacementImage = "image"
)

// The defaults of dir and cache. Under ax, /workspace is the only durable
// path. Elsewhere both directories are in the root of the workspace. An
// image holds its environment outside /workspace, because the volume hides
// the image there.
const (
	axDir     = "/workspace/.environment"
	axCache   = "/workspace/.cache"
	imageDir  = "/opt/environment"
	workDir   = ".environment"
	workCache = ".cache"
	volume    = "/workspace"
)

// Config is the block environment of fw.yaml.
type Config struct {
	Dir       string   `yaml:"dir" json:"dir,omitempty"`
	Cache     string   `yaml:"cache" json:"cache,omitempty"`
	Python    string   `yaml:"python" json:"python,omitempty"`
	Tools     string   `yaml:"tools" json:"tools,omitempty"`
	Prepare   []string `yaml:"prepare" json:"prepare,omitempty"`
	Placement string   `yaml:"placement" json:"placement,omitempty"`
	Brief     *bool    `yaml:"brief" json:"brief,omitempty"`
	Packages  *Lists   `yaml:"packages" json:"packages,omitempty"`
	Mirrors   *Mirrors `yaml:"mirrors" json:"mirrors,omitempty"`
	// On turns the packages on. With false, the lists are empty, the
	// prompt has no Environment section, and the tool env only lists.
	// The default is true.
	On *bool `yaml:"on" json:"on,omitempty"`
	// Ask lets the agent ask for a package through the tool env. With
	// false, add and remove refuse. The default is true.
	Ask *bool `yaml:"ask" json:"ask,omitempty"`
}

// Lists is the key packages of the block: the packages in plain lists.
// A list takes the place of a manifest. The kernel writes the manifest
// and its lock file under dir, and it owns both. A nil list means that
// the key is absent. An empty list is valid: the tool env can then add a
// package.
type Lists struct {
	// Python holds requirements for uv, such as "pyyaml>=6".
	Python *[]string `yaml:"python" json:"python,omitempty"`
	// Tools holds packages of conda-forge for pixi, such as "nodejs>=22".
	Tools *[]string `yaml:"tools" json:"tools,omitempty"`
}

// Mirrors is the key mirrors of the block: the package index of each
// manager, such as a cache inside the cluster.
type Mirrors struct {
	// Python is the index of uv and pip: UV_DEFAULT_INDEX and
	// PIP_INDEX_URL of every tool process and every install.
	Python string `yaml:"python" json:"python,omitempty"`
	// Tools is the mirror of https://conda.anaconda.org/conda-forge in
	// the configuration of pixi.
	Tools string `yaml:"tools" json:"tools,omitempty"`
}

// keys are the keys of the block. Another key is an error, so a typo stops
// the process.
var keys = []string{"dir", "cache", "python", "tools", "prepare", "placement", "brief", "packages", "mirrors", "on", "ask"}

// subKeys are the keys of the mappings in the block.
var subKeys = map[string][]string{
	"packages": {pkglist.Python, pkglist.Tools},
	"mirrors":  {pkglist.Python, pkglist.Tools},
}

// spec is the resolved block: every path is absolute.
type spec struct {
	root      string // the root of the workspace
	dir       string
	cache     string
	python    string // the uv manifest (pyproject.toml), or ""
	tools     string // the pixi manifest (pixi.toml or pyproject.toml), or ""
	prepare   []string
	placement string
	brief     bool
	user      *fw.ToolUser // the tool user, or nil for the user of fw
	// inline holds the configured lists of environment.packages by
	// manager (pkglist.Python, pkglist.Tools). A manager without its key
	// is absent from the map, and a manager with an empty list is present.
	inline map[string][]string
	// mirrors holds the URLs of environment.mirrors by manager.
	mirrors map[string]string
	// manifest is true for a manager whose packages a manifest of the
	// block names, and not a list.
	manifest map[string]bool
	// on and ask are the switch of the packages: the keys on and ask of
	// the block, or the push of the front (pushed).
	on, ask bool
	// pushed is true when the push of the front (.fw-env-pushed.json)
	// is the base, in place of the lists and the switch of the block.
	pushed bool
}

// load resolves the block of cfg. It returns nil and no error when cfg has
// no block.
func load(cfg *fw.Config) (*spec, error) {
	if cfg == nil {
		return nil, nil
	}
	n, ok := cfg.Extensions[Block]
	if !ok || n.Kind == 0 {
		return nil, nil
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if !slices.Contains(keys, k) {
				return nil, fmt.Errorf("env: %s.%s is not a key; the keys are %s", Block, k, strings.Join(keys, ", "))
			}
			sub, v := subKeys[k], n.Content[i+1]
			if sub == nil || v.Kind != yaml.MappingNode {
				continue
			}
			for j := 0; j+1 < len(v.Content); j += 2 {
				if kk := v.Content[j].Value; !slices.Contains(sub, kk) {
					return nil, fmt.Errorf("env: %s.%s.%s is not a key; the keys are %s", Block, k, kk, strings.Join(sub, ", "))
				}
			}
		}
	}
	var c Config
	if _, err := cfg.Extension(Block, &c); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	return resolve(cfg, c)
}

// resolve fills the defaults of c and checks the paths.
func resolve(cfg *fw.Config, c Config) (*spec, error) {
	root, err := filepath.Abs(cfg.WorkspaceRoot())
	if err != nil {
		return nil, fmt.Errorf("env: the root of the workspace: %w", err)
	}
	s := &spec{root: root, brief: c.Brief == nil || *c.Brief, on: c.On == nil || *c.On, ask: c.Ask == nil || *c.Ask}
	switch p := strings.ToLower(strings.TrimSpace(c.Placement)); p {
	case "", PlacementDurable:
		s.placement = PlacementDurable
	case PlacementImage:
		s.placement = PlacementImage
	default:
		return nil, fmt.Errorf("env: %s.placement %q is not %s or %s", Block, c.Placement, PlacementDurable, PlacementImage)
	}
	// The volume of ax decides the defaults, as it decides the default of
	// FW_HOME (fw.DefaultHome).
	onVolume := isDir(volume)
	dir, cache := filepath.Join(root, workDir), filepath.Join(root, workCache)
	if onVolume {
		dir, cache = axDir, axCache
	}
	if s.placement == PlacementImage {
		dir = imageDir
	}
	s.dir, s.cache = s.path(c.Dir, dir), s.path(c.Cache, cache)
	s.manifest = map[string]bool{pkglist.Python: c.Python != "", pkglist.Tools: c.Tools != ""}
	if c.Python != "" {
		s.python = s.path(c.Python, "")
		if filepath.Base(s.python) != "pyproject.toml" {
			return nil, fmt.Errorf("env: %s.python %s is not a pyproject.toml file, which uv needs", Block, c.Python)
		}
	}
	if c.Tools != "" {
		s.tools = s.path(c.Tools, "")
		if b := filepath.Base(s.tools); b != "pixi.toml" && b != "pyproject.toml" {
			return nil, fmt.Errorf("env: %s.tools %s is not a pixi.toml or a pyproject.toml file, which pixi needs", Block, c.Tools)
		}
	}
	for i, p := range c.Prepare {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("env: %s.prepare[%d] is empty", Block, i)
		}
		s.prepare = append(s.prepare, p)
	}
	if err := s.lists(c); err != nil {
		return nil, err
	}
	if err := s.check(cfg, onVolume); err != nil {
		return nil, err
	}
	if s.user, err = fw.ToolUserOf(cfg); err != nil {
		return nil, fmt.Errorf("env: %w", err)
	}
	s.base()
	return s, nil
}

// lists checks the keys packages and mirrors of c and fills inline and
// mirrors. A list takes the place of the manifest of its manager, so the
// two cannot both exist. A list or a mirror that does not fit stops the
// start.
func (s *spec) lists(c Config) error {
	if c.Packages != nil {
		for _, l := range []struct {
			manager, file string
			list          *[]string
		}{{pkglist.Python, c.Python, c.Packages.Python}, {pkglist.Tools, c.Tools, c.Packages.Tools}} {
			if l.list == nil {
				continue
			}
			if l.file != "" {
				return fmt.Errorf("env: %s.packages.%s and %s.%s cannot both exist; name the packages in the list or in the manifest", Block, l.manager, Block, l.manager)
			}
			if err := pkglist.Check(l.manager, *l.list); err != nil {
				return fmt.Errorf("env: %s.packages.%s %w", Block, l.manager, err)
			}
			list := make([]string, 0, len(*l.list))
			for _, req := range *l.list {
				list = append(list, strings.TrimSpace(req))
			}
			if s.inline == nil {
				s.inline = map[string][]string{}
			}
			s.inline[l.manager] = list
		}
		// The kernel writes the manifest of a list under dir. The rest
		// of the package reads it as it reads a manifest of the block.
		if s.isInline(pkglist.Python) {
			s.python = filepath.Join(s.srcDir(pkglist.Python), "pyproject.toml")
		}
		if s.isInline(pkglist.Tools) {
			s.tools = filepath.Join(s.srcDir(pkglist.Tools), "pixi.toml")
		}
		if s.inline != nil && s.placement == PlacementImage {
			return fmt.Errorf("env: %s.packages needs placement durable; for placement image, name a manifest with its lock file", Block)
		}
	}
	if c.Mirrors != nil {
		for _, m := range []struct{ manager, url string }{{pkglist.Python, c.Mirrors.Python}, {pkglist.Tools, c.Mirrors.Tools}} {
			u := strings.TrimSpace(m.url)
			if u == "" {
				continue
			}
			if err := pkglist.CheckMirror(u); err != nil {
				return fmt.Errorf("env: %s.mirrors.%s %w", Block, m.manager, err)
			}
			if s.mirrors == nil {
				s.mirrors = map[string]string{}
			}
			s.mirrors[m.manager] = strings.TrimRight(u, "/")
		}
	}
	return nil
}

// path makes p absolute against the root of the workspace, or returns def
// for an empty p.
func (s *spec) path(p, def string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return def
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.root, p)
	}
	return filepath.Clean(p)
}

// check refuses a directory that the kernel or a run owns, and a durable
// directory that a suspend loses. The directory of the environment must be
// a sibling of FW_HOME: never FW_HOME or a path under it, never the
// workspace itself, and never the directory of a run (tools.files.perRun).
func (s *spec) check(cfg *fw.Config, onVolume bool) error {
	home := cfg.Home
	if home == "" {
		home = fw.DefaultHome()
	}
	home, err := filepath.Abs(home)
	if err != nil {
		return fmt.Errorf("env: FW_HOME: %w", err)
	}
	if s.dir == s.cache {
		return fmt.Errorf("env: %s.dir and %s.cache are the same directory %s", Block, Block, s.dir)
	}
	for _, p := range []struct{ key, path string }{{"dir", s.dir}, {"cache", s.cache}} {
		switch {
		case under(p.path, home) || under(home, p.path):
			return fmt.Errorf("env: %s.%s %s overlaps FW_HOME %s; use a sibling of FW_HOME, such as %s", Block, p.key, p.path, home, filepath.Join(filepath.Dir(home), workDir))
		case under(s.root, p.path):
			return fmt.Errorf("env: %s.%s %s holds the root of the workspace %s; use a directory of its own", Block, p.key, p.path, s.root)
		case cfg.Tools.Files.PerRunOn() && under(p.path, filepath.Join(s.root, fw.RunsDir)):
			return fmt.Errorf("env: %s.%s %s is under the directories of the runs (tools.files.perRun); use a directory outside %s", Block, p.key, p.path, filepath.Join(s.root, fw.RunsDir))
		}
	}
	if onVolume && s.placement == PlacementDurable && !under(s.dir, volume) {
		return fmt.Errorf("env: %s.dir %s is outside %s, so every suspend loses it; use %s, or placement: image", Block, s.dir, volume, axDir)
	}
	if s.placement == PlacementImage && under(s.dir, volume) {
		return fmt.Errorf("env: %s.dir %s is under %s, where the volume of the Task hides the image; use a directory such as %s for placement: image", Block, s.dir, volume, imageDir)
	}
	return nil
}

// under reports whether p is dir or a path below it.
func under(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// The layout of the environment. The kernel owns dir and the files that
// record the state. The tool user owns every directory below that the
// managers write.
func (s *spec) venv() string        { return filepath.Join(s.dir, "venv") }
func (s *spec) pythonDir() string   { return filepath.Join(s.dir, "python") }
func (s *spec) binDir() string      { return filepath.Join(s.dir, "bin") }
func (s *spec) uvToolDir() string   { return filepath.Join(s.dir, "uv-tools") }
func (s *spec) pixiHome() string    { return filepath.Join(s.dir, "pixi") }
func (s *spec) pixiEnvs() string    { return filepath.Join(s.dir, "pixi-envs") }
func (s *spec) uvCache() string     { return filepath.Join(s.cache, "uv") }
func (s *spec) pixiCache() string   { return filepath.Join(s.cache, "pixi") }
func (s *spec) markerPath() string  { return filepath.Join(s.dir, ".fw-env.json") }
func (s *spec) failurePath() string { return filepath.Join(s.dir, ".fw-env-failed.json") }
func (s *spec) lockPath() string    { return filepath.Join(s.dir, ".fw-env.lock") }
func (s *spec) uvLock() string      { return filepath.Join(filepath.Dir(s.python), "uv.lock") }
func (s *spec) pixiLock() string    { return filepath.Join(filepath.Dir(s.tools), "pixi.lock") }

// imagePixiPrefix is the pixi environment of an image. The Dockerfile
// copies the manifest to a fixed directory under dir, and pixi installs
// the environment next to it.
func (s *spec) imagePixiPrefix() string {
	return filepath.Join(s.dir, "src", "tools", ".pixi", "envs", "default")
}

// owned lists the directories that the tool user owns, in the order of
// creation.
func (s *spec) owned() []string {
	return []string{s.venv(), s.pythonDir(), s.binDir(), s.uvToolDir(), s.pixiHome(), s.pixiEnvs(), s.cache, s.uvCache(), s.pixiCache()}
}

// rel names a path of the workspace relative to its root, for a person
// and for the model.
func (s *spec) rel(p string) string {
	if r, err := filepath.Rel(s.root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

// setList makes list the base list of manager, in place of the list of
// the block. The kernel writes its manifest under dir.
func (s *spec) setList(manager string, list []string) {
	if s.inline == nil {
		s.inline = map[string][]string{}
	}
	if list == nil {
		list = []string{}
	}
	s.inline[manager] = list
	if manager == pkglist.Python {
		s.python = filepath.Join(s.srcDir(pkglist.Python), "pyproject.toml")
	} else {
		s.tools = filepath.Join(s.srcDir(pkglist.Tools), "pixi.toml")
	}
}

// base applies the push of the front and the switch. A push replaces the
// lists and the switch of the block for each manager without a manifest.
// With the packages off, every list is empty, so the next install removes
// the environment.
func (s *spec) base() {
	if s.placement != PlacementDurable {
		return
	}
	if p := s.readPushed(); p != nil {
		s.on, s.ask, s.pushed = p.On, p.Ask, true
		for _, l := range []struct {
			manager string
			list    []string
		}{{pkglist.Python, p.Python}, {pkglist.Tools, p.Tools}} {
			if !s.manifest[l.manager] {
				s.setList(l.manager, l.list)
			}
		}
	}
	if !s.on {
		s.setList(pkglist.Python, nil)
		s.setList(pkglist.Tools, nil)
	}
}

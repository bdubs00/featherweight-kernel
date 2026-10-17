package config

// This file is the profile store: one YAML file per profile under
// <home>/agents, which loadProfiles reads at the next start. The kernel
// calls the store from Agent.PutProfile and Agent.DeleteProfile, so a
// profile that a dashboard or fw agents changes outlives the process.
//
// File format. A file holds the fields of fw.AgentProfile under their yaml
// keys, with name set to the profile name, and system and instructions as
// the texts themselves. The loader treats a system or instructions value as
// a path only when a file of that name exists, so a one-line prompt that
// happens to name an existing file comes back as the contents of that file.
// A prompt with a line break never does. The loader also expands ${NAME}
// and reads $$ as one $, so the store writes $${ for ${ and $$$$ for $$,
// and a text comes back as it was put.
//
// Origin rule. A profile that the configuration defines, in fw.yaml, in a
// FW_AGENTS_* variable, or through a Go option, wins over a file of the
// same name at the next load, so a change to it through the store is lost
// at the next start. The store refuses such a change with an error that
// says so. loadProfiles records which names it read from files, and the
// store treats every other name that the configuration holds when the
// store opens as fixed.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"weak"

	"github.com/bdubs00/featherweight-kernel/fw"
	"gopkg.in/yaml.v3"
)

func init() {
	fw.ProfileStoreOpener = OpenProfileStore
}

// origins records, per loaded configuration, the profile names that
// loadProfiles read from files under <home>/agents. The key is a weak
// pointer, so a configuration that nothing holds any more falls out of
// the map at the next record. This is the one piece of state that the
// loader and the store share.
var origins = struct {
	mu sync.Mutex
	m  map[weak.Pointer[fw.Config]]map[string]bool
}{m: map[weak.Pointer[fw.Config]]map[string]bool{}}

// recordOrigins remembers the names that came from files for cfg.
func recordOrigins(cfg *fw.Config, fromFiles map[string]bool) {
	origins.mu.Lock()
	defer origins.mu.Unlock()
	for k := range origins.m {
		if k.Value() == nil {
			delete(origins.m, k)
		}
	}
	origins.m[weak.Make(cfg)] = fromFiles
}

// fileOrigins returns the names that loadProfiles read from files for cfg,
// or nil for a configuration that the loader did not build.
func fileOrigins(cfg *fw.Config) map[string]bool {
	origins.mu.Lock()
	defer origins.mu.Unlock()
	return origins.m[weak.Make(cfg)]
}

// fileStore is the profile store: one YAML file per profile under dir.
type fileStore struct {
	cfg   *fw.Config
	dir   string
	fixed map[string]bool // the names that the configuration defines

	mu    sync.Mutex
	files map[string]string // profile name to its file, for the files that exist
}

// OpenProfileStore returns the store of a configuration: one YAML file per
// profile under <home>/agents. fw.New calls it through
// fw.ProfileStoreOpener.
func OpenProfileStore(cfg *fw.Config) (fw.ProfileStore, error) {
	home := cfg.Home
	if home == "" {
		home = fw.DefaultHome()
	}
	fromFiles := fileOrigins(cfg)
	fixed := map[string]bool{}
	for name := range cfg.Agents {
		if !fromFiles[name] {
			fixed[name] = true
		}
	}
	s := &fileStore{cfg: cfg, dir: filepath.Join(home, "agents"), fixed: fixed}
	files, err := profileFiles(s.dir)
	if err != nil {
		return nil, err
	}
	s.files = files
	return s, nil
}

// profileFiles maps each profile name under dir to its file: the name
// field of the file when it has one, else the file name without its
// extension. When two files name the same profile, the first in sorted
// order wins, as it does in loadProfiles.
func profileFiles(dir string) (map[string]string, error) {
	var paths []string
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	out := map[string]string{}
	for _, p := range paths {
		name := profileFileName(p)
		if _, ok := out[name]; !ok {
			out[name] = p
		}
	}
	return out, nil
}

// profileFileName returns the name that a profile file declares, else the
// file name without its extension.
func profileFileName(path string) string {
	var head struct {
		Name string `yaml:"name"`
	}
	if data, err := os.ReadFile(path); err == nil {
		_ = yaml.Unmarshal(data, &head)
	}
	if head.Name != "" {
		return head.Name
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// errFixed words the refusal to change a profile that the configuration
// defines.
func (s *fileStore) errFixed(name, verb string) error {
	return fmt.Errorf("config: cannot %s the profile %s: the configuration defines it (fw.yaml, a FW_AGENTS_* variable, or a Go option), and the configuration wins over %s at the next load; change it there", verb, name, s.dir)
}

// Put implements fw.ProfileStore. It validates the profile as Validate
// does, so the next load accepts the file, and writes the file to a
// temporary name first, so a crash leaves the old file whole.
func (s *fileStore) Put(_ context.Context, p fw.AgentProfile) error {
	if p.Name == "" {
		return errors.New("config: a profile needs a name")
	}
	if s.fixed[p.Name] {
		return s.errFixed(p.Name, "change")
	}
	if problems := profileProblems(s.cfg, p.Name, p); len(problems) > 0 {
		return fmt.Errorf("config: profile %s: %s", p.Name, strings.Join(problems, "; "))
	}
	data, err := encodeProfile(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.files[p.Name]
	if !ok {
		path = filepath.Join(s.dir, p.Name+".yaml")
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// The temporary name ends in .tmp, which the loader does not read.
	tmp, err := os.CreateTemp(s.dir, "."+p.Name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := writeAll(tmp, data); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	s.files[p.Name] = path
	return nil
}

// writeAll sets the mode of f to 0600, writes data, syncs, and closes.
func writeAll(f *os.File, data []byte) error {
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Delete implements fw.ProfileStore. A profile with no file is deleted
// already.
func (s *fileStore) Delete(_ context.Context, name string) error {
	if s.fixed[name] {
		return s.errFixed(name, "delete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.files[name]
	if !ok {
		path = filepath.Join(s.dir, name+".yaml")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: %w", err)
	}
	delete(s.files, name)
	return nil
}

// encodeProfile returns the YAML of a profile with every $ sequence that
// the loader reads specially escaped, so the texts load back as they are.
// An empty skills list means no skill, while an absent one means every
// skill, so the empty list is written although its tag says omitempty.
func encodeProfile(p fw.AgentProfile) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(p); err != nil {
		return nil, fmt.Errorf("config: encode profile %s: %w", p.Name, err)
	}
	if p.Skills != nil && len(p.Skills) == 0 && n.Kind == yaml.MappingNode {
		n.Content = append(n.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "skills"},
			&yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle})
	}
	escapeNode(&n)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return nil, fmt.Errorf("config: encode profile %s: %w", p.Name, err)
	}
	return out, nil
}

// escapeNode escapes the scalars under n for expand: ${ becomes $${ and
// $$ becomes $$$$. Any other $ stays, because expand keeps it.
func escapeNode(n *yaml.Node) {
	switch n.Kind {
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "$") {
			n.Value = escapeDollars(n.Value)
		}
	default:
		for _, c := range n.Content {
			escapeNode(c)
		}
	}
}

func escapeDollars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '$' && i+1 < len(s) {
			switch s[i+1] {
			case '{':
				b.WriteString("$${")
				i++
				continue
			case '$':
				b.WriteString("$$$$")
				i++
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// LoadProfile reads one profile file the way the loader reads a file under
// <home>/agents: ${NAME} expands from the environment, a key that the
// profile does not know is an error, and a system or instructions value
// that names a file next to the profile, or relative to the working
// directory, is replaced by the contents of that file. The name is the
// name field of the file, else the file name without its extension.
func LoadProfile(path string) (fw.AgentProfile, error) {
	var p fw.AgentProfile
	env := parseEnviron(os.Environ())
	if err := decodeFile(path, &p, env.lookup); err != nil {
		return p, err
	}
	if p.Name == "" {
		p.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	dir := filepath.Dir(path)
	text, err := resolvePrompt(p.System, dir)
	if err != nil {
		return p, fmt.Errorf("config: %s: system: %w", path, err)
	}
	p.System = text
	for i, s := range p.Instructions {
		text, err := resolvePrompt(s, dir)
		if err != nil {
			return p, fmt.Errorf("config: %s: instructions[%d]: %w", path, i, err)
		}
		p.Instructions[i] = text
	}
	return p, nil
}

// Locked implements fw.ProfileLocker. The store refuses a change to a
// profile that the configuration defines.
func (s *fileStore) Locked(name string) bool { return s.fixed[name] }

package fw

// This file loads skills. A skill is a directory with a SKILL.md file: a
// set of instructions that the model reads when a task needs them. The
// front matter of SKILL.md names the skill and describes it in one line.
// The kernel lists the skills of a profile in the system prompt, and the
// built-in skill tool returns the body of one skill on demand. The same
// files are MCP resources under fw://skills/<name>. A tool source that
// implements SkillSource, such as an MCP server with the skills extension,
// adds skills under the name "<server>/<name>" (docs/ARCHITECTURE.md
// section 7.3).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// SkillFile is the file that makes a directory a skill.
const SkillFile = "SKILL.md"

// Limits of a skill file.
const (
	maxSkillBytes       = 4 << 20
	maxSkillDescription = 300
)

// Skill is one skill: a directory on disk, or a skill of a SkillSource.
// Origin is empty for a skill of the skill directories and SkillOriginMCP
// for a skill of an MCP server. Server is the entry name of that server,
// and URI the URI of its SKILL.md. The name of such a skill is
// "<server>/<name>", so it never shadows a skill of the directories, and
// Dir is empty.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir"` // the directory that holds SKILL.md
	Origin      string `json:"origin,omitempty"`
	Server      string `json:"server,omitempty"`
	URI         string `json:"uri,omitempty"`
	// source reads a skill of a SkillSource at the moment of use.
	source SkillSource
}

// Origins of a skill (Skill.Origin). A skill of the skill directories has
// an empty origin, which surfaces show as SkillOriginLocal.
const (
	SkillOriginLocal = "local"
	SkillOriginMCP   = "mcp"
)

// OriginName returns the origin of the skill for a listing:
// SkillOriginLocal for a skill of the directories.
func (s Skill) OriginName() string {
	if s.Origin == "" {
		return SkillOriginLocal
	}
	return s.Origin
}

// A SkillSource is a tool source that also serves skills, such as an MCP
// server with the skills extension (docs/ARCHITECTURE.md section 7.3).
// The kernel adds its skills to the skills of the skill directories
// (Agent.Skills).
//
// SourceSkills returns the skills as the source last listed them, each
// with Name "<server>/<name>", Origin, Server, and URI set. It lists them
// on first use and again after a change; a source that cannot list keeps
// its last list. ReadSkill reads SKILL.md of a skill when file is "", or
// a supporting file by its path relative to the skill, at the moment of
// use, and verifies it: it returns an error that says why when the size,
// the digest, or the front matter do not match. SkillFiles lists the
// supporting files of a skill as the source knows them, or nil when the
// source lists them on demand.
type SkillSource interface {
	SourceSkills(ctx context.Context) []Skill
	ReadSkill(ctx context.Context, s Skill, file string) (string, error)
	SkillFiles(s Skill) []string
}

// WithSkillSource returns s as a skill of the source src, so Body, Files,
// and ReadFile read it through src. A SkillSource sets it on each skill
// that it lists.
func (s Skill) WithSkillSource(src SkillSource) Skill {
	s.source = src
	return s
}

// LoadSkills lists the skills under each directory: every subdirectory that
// holds a SKILL.md file, with a name from the front matter or else from the
// directory. A directory that holds a SKILL.md file itself is one skill. A
// directory that does not exist is skipped. The result is sorted by name,
// and the first directory wins for a name that appears twice.
func LoadSkills(dirs ...string) ([]Skill, error) {
	seen := map[string]bool{}
	var out []Skill
	var errs []error
	add := func(dir string) {
		s, err := readSkill(dir)
		if err != nil {
			errs = append(errs, err)
			return
		}
		if seen[s.Name] {
			return
		}
		seen[s.Name] = true
		out = append(out, s)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if isRegular(filepath.Join(dir, SkillFile)) {
			add(dir)
			continue
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("fw: skills: %w", err))
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if isRegular(filepath.Join(sub, SkillFile)) {
				add(sub)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errors.Join(errs...)
}

func isRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// readSkill reads the front matter of the SKILL.md file in dir.
func readSkill(dir string) (Skill, error) {
	f, err := os.Open(filepath.Join(dir, SkillFile))
	if err != nil {
		return Skill{}, fmt.Errorf("fw: skills: %w", err)
	}
	defer f.Close()
	s := Skill{Name: ValidSkillName(filepath.Base(dir)), Dir: dir}
	name, desc := frontMatter(f)
	if v := ValidSkillName(name); v != "" {
		s.Name = v
	}
	s.Description = desc
	if s.Name == "" {
		return Skill{}, fmt.Errorf("fw: skills: %s has no usable name", dir)
	}
	return s, nil
}

// SkillFrontMatter returns the name and the description of the front
// matter of the text of a SKILL.md file, as the kernel reads them for a
// skill of the directories: the name as it stands, and the description cut
// to 300 bytes. A description that is a YAML block scalar, such as
// "description: >-" with its text on the lines below, is read with YAML
// and put on one line. Both are empty for a text without front matter.
func SkillFrontMatter(text string) (name, description string) {
	return frontMatter(strings.NewReader(text))
}

// blockIndicator is the head of a YAML block scalar: | for a literal and >
// for a folded one, with an indentation digit and a chomping sign, + or -,
// in either order, and an optional comment.
var blockIndicator = regexp.MustCompile(`^[|>](?:[1-9][+-]?|[+-][1-9]?)?(?:\s+#.*)?$`)

func frontMatter(r io.Reader) (name, description string) {
	sc := bufio.NewScanner(io.LimitReader(r, 64<<10))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", ""
	}
	var fm strings.Builder
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			break
		}
		fm.WriteString(sc.Text() + "\n")
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			name = value
		case "description":
			description = CutSkillDescription(value)
		}
	}
	if blockIndicator.MatchString(description) {
		description = blockDescription(fm.String())
	}
	return name, description
}

// blockDescription reads the description of a front matter with YAML, for
// a description that is a block scalar, and returns it on one line, cut to
// 300 bytes. It is empty when the front matter is not valid YAML.
func blockDescription(fm string) string {
	var v struct {
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(fm), &v) != nil {
		return ""
	}
	return strings.TrimSpace(CutSkillDescription(strings.Join(strings.Fields(v.Description), " ")))
}

// CutSkillDescription cuts the description of a skill to 300 bytes at a
// rune boundary.
func CutSkillDescription(d string) string {
	if len(d) <= maxSkillDescription {
		return d
	}
	i := maxSkillDescription
	for i > 0 && d[i]&0xC0 == 0x80 {
		i--
	}
	return d[:i]
}

// ValidSkillName returns the name with the characters that a skill name
// cannot hold removed, or "" when nothing is left. A name holds letters,
// digits, '-', '_', and '.', and does not start with '.'.
func ValidSkillName(name string) string {
	var sb strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		}
	}
	return strings.TrimLeft(sb.String(), ".")
}

// Body returns the text of the SKILL.md file of the skill. A file larger
// than 4 MB is cut. A skill of a SkillSource reads it through the source,
// which verifies it.
func (s Skill) Body() (string, error) {
	if s.source != nil {
		return s.source.ReadSkill(context.Background(), s, "")
	}
	f, err := os.Open(filepath.Join(s.Dir, SkillFile))
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSkillBytes))
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	return string(b), nil
}

// Files lists the files of the skill directory, relative to it, without
// SKILL.md and without hidden files, so the model knows what else the skill
// ships, such as scripts and references. A skill of a SkillSource lists
// the files that the source knows.
func (s Skill) Files() ([]string, error) {
	if s.source != nil {
		return s.source.SkillFiles(s), nil
	}
	var out []string
	root := os.DirFS(s.Dir)
	err := walkDir(root, ".", func(p string, isDir bool) {
		if p == "." || p == SkillFile || strings.HasPrefix(path.Base(p), ".") || isDir {
			return
		}
		out = append(out, p)
	})
	if err != nil {
		return nil, fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	sort.Strings(out)
	return out, nil
}

// ReadFile reads a file of the skill: SKILL.md when file is "", else a
// supporting file by its path relative to the skill. A path that leaves
// the skill, an absolute path, and a hidden file are refused. A file is
// cut at 4 MB. A skill of a SkillSource reads the file through the source
// at the moment of use, and the source verifies it.
func (s Skill) ReadFile(ctx context.Context, file string) (string, error) {
	file = strings.TrimSpace(file)
	if file == "" || file == SkillFile {
		if s.source != nil {
			return s.source.ReadSkill(ctx, s, "")
		}
		return s.Body()
	}
	clean, err := SkillFilePath(file)
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	if s.source != nil {
		return s.source.ReadSkill(ctx, s, clean)
	}
	root, err := os.OpenRoot(s.Dir)
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(clean))
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSkillBytes))
	if err != nil {
		return "", fmt.Errorf("fw: skill %s: %w", s.Name, err)
	}
	return string(b), nil
}

// SkillFilePath cleans the path of a supporting file of a skill and
// refuses a path that is absolute, leaves the skill, or names a hidden
// file.
func SkillFilePath(file string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(strings.TrimSpace(file), "\\", "/"))
	switch {
	case clean == "." || clean == "":
		return "", errors.New("the file has no name")
	case path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("the file %q is outside the skill", file)
	}
	for _, part := range strings.Split(clean, "/") {
		if strings.HasPrefix(part, ".") {
			return "", fmt.Errorf("the file %q is hidden", file)
		}
	}
	return clean, nil
}

// SkillAllowed reports whether a skill name matches the globs of a profile.
// A nil list allows every skill, and an empty list allows none. A glob
// matches a skill of a server by its qualified name, so "github/*" allows
// every skill of the server github.
func SkillAllowed(globs []string, name string) bool {
	if globs == nil {
		return true
	}
	for _, g := range globs {
		if g == "*" || g == name {
			return true
		}
		if ok, err := path.Match(g, name); err == nil && ok {
			return true
		}
	}
	return false
}

// Skills returns the skills of the agent: the skills of the directories
// that Config.SkillDirs names, sorted by name, then the skills of each tool
// source that implements SkillSource, in the order of the sources. It
// reads the directories on every call, so a skill added while the process
// runs is in the next system prompt. A read error is logged and leaves the
// skills that could be read. A skill of a source has a qualified name, so
// it never shadows a skill of the directories; a second skill of a source
// under a name that is already listed is left out.
func (a *Agent) Skills() []Skill {
	skills, err := LoadSkills(a.cfg.SkillDirs()...)
	if err != nil {
		a.log.Warn("load skills", "error", err)
	}
	seen := make(map[string]bool, len(skills))
	for _, s := range skills {
		seen[s.Name] = true
	}
	for _, src := range a.sources {
		ss, ok := src.(SkillSource)
		if !ok {
			continue
		}
		for _, s := range ss.SourceSkills(context.Background()) {
			if s.Name == "" || seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			if s.source == nil {
				s.source = ss
			}
			skills = append(skills, s)
		}
	}
	return skills
}

// warmSkills lists the skills of each skill source in the background when
// the kernel starts, so the first run lists them. A source that does not
// answer keeps trying at the next listing.
func (a *Agent) warmSkills() {
	for _, src := range a.sources {
		if ss, ok := src.(SkillSource); ok {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				ss.SourceSkills(ctx)
			}()
		}
	}
}

// SkillNamed returns the skill with that name.
func (a *Agent) SkillNamed(name string) (Skill, bool) {
	for _, s := range a.Skills() {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// walkDir walks a file system and calls fn for each entry. Hidden
// directories are skipped.
func walkDir(fsys fs.FS, root string, fn func(p string, isDir bool)) error {
	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		fn(p, d.IsDir())
		return nil
	})
}

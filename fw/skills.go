package fw

// This file loads skills. A skill is a directory with a SKILL.md file: a
// set of instructions that the model reads when a task needs them. The
// front matter of SKILL.md names the skill and describes it in one line.
// The kernel lists the skills of a profile in the system prompt, and the
// built-in skill tool returns the body of one skill on demand. The same
// files are MCP resources under fw://skills/<name>.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SkillFile is the file that makes a directory a skill.
const SkillFile = "SKILL.md"

// Limits of a skill file.
const (
	maxSkillBytes       = 4 << 20
	maxSkillDescription = 300
)

// Skill is one skill on disk.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Dir         string `json:"dir"` // the directory that holds SKILL.md
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
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return s, nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch strings.TrimSpace(key) {
		case "name":
			if v := ValidSkillName(value); v != "" {
				s.Name = v
			}
		case "description":
			if len(value) > maxSkillDescription {
				value = value[:maxSkillDescription]
			}
			s.Description = value
		}
	}
	if s.Name == "" {
		return Skill{}, fmt.Errorf("fw: skills: %s has no usable name", dir)
	}
	return s, nil
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
// than 4 MB is cut.
func (s Skill) Body() (string, error) {
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
// ships, such as scripts and references.
func (s Skill) Files() ([]string, error) {
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

// SkillAllowed reports whether a skill name matches the globs of a profile.
// A nil list allows every skill, and an empty list allows none.
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

// Skills returns the skills of the agent, from the directories that
// Config.SkillDirs names. It reads the directories on every call, so a
// skill added while the process runs is in the next system prompt. A read
// error is logged and leaves the skills that could be read.
func (a *Agent) Skills() []Skill {
	skills, err := LoadSkills(a.cfg.SkillDirs()...)
	if err != nil {
		a.log.Warn("load skills", "error", err)
	}
	return skills
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

package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of the index of the notes at the start of a run.
const (
	IndexMaxNotes = 40      // notes in the index
	IndexMaxBytes = 4 << 10 // bytes of the lines of the index
	lineReadMax   = 2 << 10 // bytes read from each note for its first line
	LineMax       = 160     // characters of one first line
)

// note is one note of a directory.
type note struct {
	name  string // slash-separated, relative to the directory
	mod   time.Time
	size  int64
	ino   uint64
	title string
}

// walkNotes lists the notes of dir, newest first, and reads the first line
// of the first titles of them, or of every note when titles is negative.
// It visits at most maxWalk entries. A missing directory has no notes.
// Every read goes through os.Root, so a symbolic link cannot leave the
// directory.
func walkNotes(ctx context.Context, dir string, titles int) ([]note, error) {
	r, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	fsys := r.FS()
	var notes []note
	seen := 0
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == "." {
				return err
			}
			return nil // an unreadable entry leaves the list
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		seen++
		if seen > maxWalk {
			return fs.SkipAll
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		notes = append(notes, note{name: p, mod: info.ModTime(), size: info.Size(), ino: inodeOf(info)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(notes, func(a, b note) int {
		if c := b.mod.Compare(a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	for i := range notes {
		if titles >= 0 && i >= titles {
			break
		}
		notes[i].title = firstLine(fsys, notes[i].name)
	}
	return notes, nil
}

// Index returns the lines of the index of a scope, "- /memories/<path>:
// <first line>", newest first, within IndexMaxNotes and IndexMaxBytes.
// more counts the notes that do not fit. It creates nothing.
func (s *Store) Index(ctx context.Context, scope fw.MemoryScope) (lines []string, more int, err error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, 0, err
	}
	notes, err := walkNotes(ctx, dir, IndexMaxNotes)
	if err != nil {
		return nil, 0, fmt.Errorf("memory: index of %s: %w", dir, err)
	}
	size := 0
	for _, n := range notes {
		line := "- " + Prefix + "/" + n.name
		if n.title != "" {
			line += ": " + n.title
		}
		if len(lines) == IndexMaxNotes || (len(lines) > 0 && size+len(line)+1 > IndexMaxBytes) {
			break
		}
		lines = append(lines, line)
		size += len(line) + 1
	}
	return lines, len(notes) - len(lines), nil
}

// firstLine returns the line that says what a note holds: the description
// of a front matter block, or else the first line with text, without its
// leading '#' marks. It reads at most lineReadMax bytes and cuts the line
// to LineMax characters. A file that cannot be read has no line.
func firstLine(fsys fs.FS, name string) string {
	f, err := fsys.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, lineReadMax))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ToValidUTF8(string(buf), ""), "\n")
	i := 0
	if strings.TrimSpace(lines[0]) == "---" {
		for i = 1; i < len(lines); i++ {
			l := strings.TrimSpace(lines[i])
			if l == "---" {
				i++
				break
			}
			if v, ok := strings.CutPrefix(l, "description:"); ok {
				if v = strings.Trim(strings.TrimSpace(v), `"'`); v != "" {
					return ClipLine(v)
				}
			}
		}
	}
	for ; i < len(lines); i++ {
		if l := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "#")); l != "" {
			return ClipLine(l)
		}
	}
	return ""
}

// ClipLine replaces control characters with spaces and cuts s to LineMax
// characters, with "..." after a cut.
func ClipLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= LineMax {
		return s
	}
	n := 0
	for i := range s {
		if n == LineMax {
			return s[:i] + "..."
		}
		n++
	}
	return s
}

// readBounded reads at most n bytes of a file. A missing file gives "".
func readBounded(files fs.FS, name string, n int64) string {
	f, err := files.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, n))
	if err != nil {
		return ""
	}
	return string(bytes.ToValidUTF8(b, nil))
}

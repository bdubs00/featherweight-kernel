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
// It visits at most maxWalk notes and maxWalk other entries (walkScope);
// cut reports a walk that stopped there, so notes can be missing from the
// list. A missing directory has no notes. Every read goes through os.Root,
// so a symbolic link cannot leave the directory.
func walkNotes(ctx context.Context, dir string, titles int) (notes []note, cut bool, err error) {
	r, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	fsys := r.FS()
	cut, err = walkScope(ctx, fsys, maxWalk, func(p string, d fs.DirEntry) {
		info, err := d.Info()
		if err != nil {
			return // a note that went during the walk leaves the list
		}
		notes = append(notes, note{name: p, mod: info.ModTime(), size: info.Size(), ino: inodeOf(info)})
	})
	if err != nil {
		return nil, false, err
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
	return notes, cut, nil
}

// Index returns the lines of the index of a scope, "- /memories/<path>:
// <first line>", newest first, within IndexMaxNotes and IndexMaxBytes.
// more counts the notes that do not fit. cut reports a walk that stopped
// at its bound, so more is too low and notes can be missing (CutNote). A
// path is escaped (EscapeName), so a file that a person put in the
// directory with a newline in its name cannot forge a line. It creates
// nothing.
func (s *Store) Index(ctx context.Context, scope fw.MemoryScope) (lines []string, more int, cut bool, err error) {
	shown, more, cut, err := s.indexNotes(ctx, scope)
	for _, n := range shown {
		lines = append(lines, indexLine(n, ""))
	}
	return lines, more, cut, err
}

// indexNotes returns the notes that the index of a scope shows, newest
// first, within IndexMaxNotes and IndexMaxBytes, with the count of the
// notes left out and whether the walk stopped at its bound.
func (s *Store) indexNotes(ctx context.Context, scope fw.MemoryScope) (shown []note, more int, cut bool, err error) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, 0, false, err
	}
	notes, cut, err := walkNotes(ctx, dir, IndexMaxNotes)
	if err != nil {
		return nil, 0, false, fmt.Errorf("memory: index of %s: %w", dir, err)
	}
	size := 0
	for _, n := range notes {
		line := indexLine(n, "")
		if len(shown) == IndexMaxNotes || (len(shown) > 0 && size+len(line)+1 > IndexMaxBytes) {
			break
		}
		shown = append(shown, n)
		size += len(line) + 1
	}
	return shown, len(notes) - len(shown), cut, nil
}

// indexLine returns the line of a note in the index: "- /memories/<path>:
// <first line>", with the label after it, such as "(from a person)", when
// label is not empty. The path is escaped (EscapeName).
func indexLine(n note, label string) string { return indexLineAt(Prefix, n, label) }

// indexLineAt is indexLine for a note whose path for the model starts with
// prefix, such as AgentPrefix for a note of the agent.
func indexLineAt(prefix string, n note, label string) string {
	line := "- " + prefix + "/" + EscapeName(n.name)
	if n.title != "" {
		line += ": " + n.title
	}
	if label != "" {
		line += " " + label
	}
	return line
}

// CutNote is the sentence that ends a listing of the notes whose walk
// stopped at its bound (walkScope), so notes can be missing from it.
var CutNote = fmt.Sprintf("The list stopped after %d entries of the memory directory, so some notes are not listed.", maxWalk)

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

// EscapeName returns a note name for a line of text: each control
// character, each line or paragraph separator, and each backslash becomes
// an escape such as \x0a or \u2028, so the name stays on its line. A name
// that CheckName accepts and that holds no backslash comes back as it is.
func EscapeName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(name, "\ufffd") {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x80 && unicode.IsControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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

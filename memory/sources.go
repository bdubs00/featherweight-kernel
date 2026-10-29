package memory

// This file holds the sources that a note names in its own text
// (docs/KNOWHOW.md section 4.2). A model has no field for a source in the
// memory tool, whose shape the provider defines, so a note names where it
// came from in a line: "Source: page:launch/plan.md" for a page of the
// wiki, or "Source: file:docs/setup.md" for a file of the workspace. A
// write without sources of its own takes the sources of these lines. With
// a front, the store asks the front for the hash of each page, so the note
// goes out of date when the page changes.

import (
	"context"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// The kinds of a source that a line of a note names.
const (
	// SourcePage starts the ref of a page of the wiki of the front.
	SourcePage = "page:"
	// SourceFile starts the ref of a file of the workspace.
	SourceFile = "file:"
)

// MaxTextSources bounds the sources that the lines of one note name.
const MaxTextSources = 8

// maxSourceRef bounds the ref of one source, as the front does.
const maxSourceRef = 512

// SourcesOf returns the sources that the text of a note names. Each line
// that starts with "Source:" or "Sources:", in any case and after a mark
// of a list, holds one or more refs: page:<path> or file:<path>, split by
// commas or spaces. A path of a page loses its leading '/'. Other words on
// the line, a ref of another kind, and a ref past MaxTextSources are left
// out. Each source has the kind "source" and no version. A text without
// such a line gives nil.
func SourcesOf(text string) []fw.MemoryLink {
	var out []fw.MemoryLink
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(strings.TrimSpace(line), "-*> ")
		head, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if h := strings.ToLower(strings.TrimSpace(head)); h != "source" && h != "sources" {
			continue
		}
		for _, tok := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			ref, ok := SourceRef(tok)
			if !ok || seen[ref] {
				continue
			}
			if len(out) == MaxTextSources {
				return out
			}
			seen[ref] = true
			out = append(out, fw.MemoryLink{Ref: ref, Kind: "source"})
		}
	}
	return out
}

// SourceRef returns the ref of one word of a line of sources: page:<path>
// or file:<path>, with the kind in lower case, without the marks that end
// a sentence or wrap a word, and for a page without a leading '/'. Another
// word gives false. The hosted fw-memory server checks the field sources
// of remember with it too.
func SourceRef(tok string) (string, bool) {
	tok = strings.Trim(tok, "`'\"()[]<>")
	tok = strings.TrimRight(tok, ".;")
	for _, kind := range []string{SourcePage, SourceFile} {
		if len(tok) <= len(kind) || !strings.EqualFold(tok[:len(kind)], kind) {
			continue
		}
		p := tok[len(kind):]
		if kind == SourcePage {
			p = strings.TrimLeft(p, "/")
		}
		ref := kind + p
		if p == "" || len(ref) > maxSourceRef {
			return "", false
		}
		return ref, true
	}
	return "", false
}

// versioned returns links with the version of each page source that has
// none: the hash of the page at the front now. Without a front, a page
// that the front does not have, or a front that does not answer, the
// source stays without a version, and the write goes on. The note then
// names its page, and it does not go out of date when the page changes.
func (s *Store) versioned(ctx context.Context, links []fw.MemoryLink) []fw.MemoryLink {
	if s.front == nil {
		return links
	}
	var out []fw.MemoryLink
	for i, l := range links {
		path, ok := strings.CutPrefix(l.Ref, SourcePage)
		if !ok || l.Version != "" {
			continue
		}
		if out == nil {
			out = append([]fw.MemoryLink(nil), links...)
		}
		pctx, cancel := context.WithTimeout(ctx, pushWait)
		hash, found, err := s.front.pageHash(pctx, path)
		cancel()
		if err == nil && found {
			out[i].Version = hash
		}
	}
	if out == nil {
		return links
	}
	return out
}

package memory

// This file holds the sources that a note names in its own text
// (docs/KNOWHOW.md section 4.2). A model has no field for a source in the
// memory tool, whose shape the provider defines, so a note names where it
// came from in a line: "Source: page:launch/plan.md" for a page of the
// wiki, or "Source: file:docs/setup.md" for a file of the workspace. A
// write without sources of its own takes the sources of these lines. With
// a front, the store asks the front for the hash of each page, so the note
// goes out of date when the page changes.
//
// A model does not always write the ref. Haiku wrote "Source: wiki page
// launch/quillwork-3.md" on the stack. So a line of sources also takes
// loose forms of a page: a path after the word page or wiki, a path of the
// wiki with or without .md, and a markdown link to a page. Only the front
// knows its pages, so the store keeps such a form as page:<path> only when
// the front has that page (Store.textSources). It never guesses a page.

import (
	"context"
	"regexp"
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
	out, _ := parseSources(text)
	return out
}

// pageWords are the words that a model writes before the path of a page,
// as in "wiki page launch/plan.md". They name no page themselves.
var pageWords = map[string]bool{"page": true, "pages": true, "wiki": true}

// mdLink matches a markdown link: its text, then its target.
var mdLink = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)

// parseSources returns the sources of the lines of sources of the text,
// as SourcesOf does, and the paths of the pages that the same lines name
// in a loose form (loosePage), at most MaxTextSources of each. The target
// of a markdown link counts as a word of the line.
func parseSources(text string) (refs []fw.MemoryLink, loose []string) {
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
		var links []string
		rest = mdLink.ReplaceAllStringFunc(rest, func(m string) string {
			links = append(links, mdLink.FindStringSubmatch(m)[1])
			return " "
		})
		words := strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		prev := ""
		for i, tok := range append(words, links...) {
			word := strings.ToLower(strings.Trim(tok, "`'\"()[]<>.;:"))
			ref, ok := SourceRef(tok)
			switch {
			case ok && !seen[ref] && len(refs) < MaxTextSources:
				seen[ref] = true
				refs = append(refs, fw.MemoryLink{Ref: ref, Kind: "source"})
			case !ok && !pageWords[word]:
				named := i >= len(words) || pageWords[prev]
				if p, ok := loosePage(tok, named); ok && !seen[SourcePage+p] && len(loose) < MaxTextSources {
					seen[SourcePage+p] = true
					loose = append(loose, p)
				}
			}
			prev = word
		}
	}
	return refs, loose
}

// loosePage returns the path of the page that one word of a line of
// sources may name, with .md: a word that has a '/' or ends with .md, or
// any word when named is true, after the word page or wiki or as the
// target of a markdown link. A link of the page of the workbench,
// #pages/<path>, names its path. A word with a ':', such as a URL or a ref
// of another kind, and a path with an empty part or "..", name no page.
func loosePage(tok string, named bool) (string, bool) {
	tok = strings.Trim(tok, "`'\"()[]<>")
	tok = strings.TrimRight(tok, ".;:!?")
	if _, after, ok := strings.Cut(tok, "#pages/"); ok {
		tok, named = after, true
	}
	tok = strings.TrimPrefix(tok, "./")
	tok = strings.TrimLeft(tok, "/")
	md := strings.HasSuffix(strings.ToLower(tok), ".md")
	if tok == "" || strings.Contains(tok, ":") || !(named || md || strings.Contains(tok, "/")) {
		return "", false
	}
	if !md {
		tok += ".md"
	}
	for _, part := range strings.Split(tok, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
	}
	if len(SourcePage+tok) > maxSourceRef {
		return "", false
	}
	return tok, true
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

// textSources returns the sources that the lines of the text name, each
// page with its version (versioned). A page that a line names in a loose
// form becomes page:<path> only when the front has that page, and then it
// has the hash of the page as its version. Without a front, or when the
// front does not answer, a loose form gives no source, because only the
// front knows its pages.
func (s *Store) textSources(ctx context.Context, text string) []fw.MemoryLink {
	refs, loose := parseSources(text)
	links := s.versioned(ctx, refs)
	if s.front == nil {
		return links
	}
	for _, p := range loose {
		if len(links) >= MaxTextSources {
			break
		}
		pctx, cancel := context.WithTimeout(ctx, pushWait)
		hash, found, err := s.front.pageHash(pctx, p)
		cancel()
		if err == nil && found {
			links = append(links, fw.MemoryLink{Ref: SourcePage + p, Kind: "source", Version: hash})
		}
	}
	return links
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

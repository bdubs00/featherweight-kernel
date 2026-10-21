package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Bounds of a recall.
const (
	DefaultLimit = 10
	MaxLimit     = 50
	maxRuns      = 500
	maxFiles     = 5000
	maxRead      = 64 << 10
	snippetLen   = 240
)

// noteIndex is the optional index of the notes. The build without the
// memory_sqlite tag has no index and scans the notes.
type noteIndex interface {
	Search(ctx context.Context, scope fw.MemoryScope, notesDir, indexDir string, terms []string, limit int) ([]fw.MemoryHit, error)
	Close() error
}

// AllKinds lists every kind that a recall searches.
var AllKinds = []fw.MemoryKind{fw.MemoryKindNote, fw.MemoryKindRun, fw.MemoryKindFile, fw.MemoryKindSkill}

// ParseKinds parses the names of kinds. An empty list means every kind.
func ParseKinds(names []string) ([]fw.MemoryKind, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out []fw.MemoryKind
	for _, name := range names {
		switch k := fw.MemoryKind(strings.ToLower(strings.TrimSpace(name))); k {
		case fw.MemoryKindNote, fw.MemoryKindRun, fw.MemoryKindFile, fw.MemoryKindSkill:
			out = append(out, k)
		case "":
		default:
			return nil, fmt.Errorf("the kind %q is not note, run, file, or skill", name)
		}
	}
	return out, nil
}

// Search implements fw.Memory. It searches the notes of the scope, the
// runs of its agent and its principal, the workspace of the run in ctx,
// and the skills of the agent in ctx. The runs, the files, and the skills
// need the agent in ctx (fw.AgentFrom); without it, only notes match.
func (s *Store) Search(ctx context.Context, scope fw.MemoryScope, q fw.MemoryQuery) ([]fw.MemoryHit, error) {
	terms := strings.Fields(strings.ToLower(q.Text))
	if len(terms) == 0 {
		return nil, errors.New("a recall needs words to find")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = AllKinds
	}
	var hits []fw.MemoryHit
	for _, k := range kinds {
		switch k {
		case fw.MemoryKindNote:
			hits = append(hits, s.searchNotes(ctx, scope, terms)...)
		case fw.MemoryKindRun:
			hits = append(hits, searchRuns(ctx, scope, terms)...)
		case fw.MemoryKindFile:
			hits = append(hits, searchFiles(ctx, terms)...)
		case fw.MemoryKindSkill:
			hits = append(hits, searchSkills(ctx, terms)...)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if !hits[i].Record.Updated.Equal(hits[j].Record.Updated) {
			return hits[i].Record.Updated.After(hits[j].Record.Updated)
		}
		return hits[i].Record.Ref < hits[j].Record.Ref
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Get implements fw.Memory. A ref starts with mem://, run://, file://, or
// skill://. A note ref resolves in the scope of the caller.
func (s *Store) Get(ctx context.Context, scope fw.MemoryScope, ref string) (fw.MemoryRecord, error) {
	switch {
	case strings.HasPrefix(ref, "mem://"):
		return s.getNote(scope, ref)
	case strings.HasPrefix(ref, "run://"):
		return getRun(ctx, scope, strings.TrimPrefix(ref, "run://"))
	case strings.HasPrefix(ref, "file://"):
		return getFile(ctx, strings.TrimPrefix(ref, "file://"))
	case strings.HasPrefix(ref, "skill://"):
		return getSkill(ctx, strings.TrimPrefix(ref, "skill://"))
	}
	return fw.MemoryRecord{}, fmt.Errorf("the ref %q must start with mem://, run://, file://, or skill://", ref)
}

// searchNotes searches the notes of the scope: through the index when the
// build has one, else by a bounded scan.
func (s *Store) searchNotes(ctx context.Context, scope fw.MemoryScope, terms []string) []fw.MemoryHit {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil
	}
	if s.index != nil {
		if idx, err := s.indexDir(scope); err == nil {
			if hits, err := s.index.Search(ctx, scope, dir, idx, terms, MaxLimit); err == nil {
				return hits
			}
		}
	}
	notes, err := walkNotes(ctx, dir, -1)
	if err != nil || len(notes) == 0 {
		return nil
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return nil
	}
	var hits []fw.MemoryHit
	for _, n := range notes {
		body := readBounded(files, n.name, maxRead)
		score, snippet := matchScore(terms, n.name, n.title, body)
		if score == 0 {
			continue
		}
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:         NoteRef(scope, n.name),
				Kind:        fw.MemoryKindNote,
				Scope:       scope,
				Title:       n.title,
				Description: n.title,
				Source:      "memory",
				Trust:       "untrusted",
				Size:        n.size,
				Updated:     n.mod,
			},
			Score:   score,
			Snippet: snippet,
			Why:     "scan",
		})
	}
	return hits
}

// searchRuns searches the runs of the agent of the scope. When the scope
// has a principal, a run of another principal never matches. It reads the
// run projection, not every event, so a search stays bounded.
func searchRuns(ctx context.Context, scope fw.MemoryScope, terms []string) []fw.MemoryHit {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return nil
	}
	runs, err := a.List(ctx)
	if err != nil {
		return nil
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
			return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
		}
		return runs[i].ID > runs[j].ID
	})
	var hits []fw.MemoryHit
	seen := 0
	for _, r := range runs {
		if !runVisible(scope, r) || r.ID == scope.Task {
			continue
		}
		if seen++; seen > maxRuns {
			break
		}
		fields := []weightedField{
			{text: r.Title, weight: 6},
			{text: r.Input, weight: 2},
			{text: r.Output, weight: 2},
		}
		for k, v := range r.Metadata {
			fields = append(fields, weightedField{text: k + " " + v, weight: 1})
		}
		score, snippet := fieldScore(terms, fields...)
		if score == 0 {
			continue
		}
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:         "run://" + r.ID,
				Kind:        fw.MemoryKindRun,
				Scope:       scope,
				Title:       r.Title,
				Description: string(r.State),
				Source:      "ledger",
				Trust:       "untrusted",
				RunID:       r.ID,
				Updated:     r.UpdatedAt,
			},
			Score:   score,
			Snippet: snippet,
			Why:     "run",
		})
	}
	return hits
}

// runVisible reports whether a run belongs to the agent of the scope and,
// when the scope has a principal, to that principal.
func runVisible(scope fw.MemoryScope, r *fw.Run) bool {
	if r == nil {
		return false
	}
	agent := r.Agent
	if agent == "" {
		agent = "default"
	}
	if scope.Agent != "" && agent != scope.Agent {
		return false
	}
	return scope.Principal == "" || r.Principal == "" || r.Principal == scope.Principal
}

// searchFiles searches the workspace of the run in ctx. It reads small text
// files for content and skips binaries. The walk and the reads are
// bounded.
func searchFiles(ctx context.Context, terms []string) []fw.MemoryHit {
	work := fw.FilesFrom(ctx)
	if work == nil {
		return nil
	}
	var hits []fw.MemoryHit
	seen := 0
	_ = fs.WalkDir(work, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if name == "." {
				return err
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		base := d.Name()
		if strings.HasPrefix(base, ".") || base == "node_modules" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if seen++; seen > maxFiles {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		body := ""
		if info.Size() <= maxRead {
			body = readBounded(work, name, maxRead)
			if bytes.IndexByte([]byte(body), 0) >= 0 {
				return nil
			}
		}
		score, snippet := matchScore(terms, name, base, body)
		if score == 0 {
			return nil
		}
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:     "file://" + name,
				Kind:    fw.MemoryKindFile,
				Title:   name,
				Source:  "workspace",
				Trust:   "untrusted",
				Size:    info.Size(),
				Updated: info.ModTime(),
			},
			Score:   score,
			Snippet: snippet,
			Why:     "file",
		})
		return nil
	})
	return hits
}

// searchSkills searches the skills of the agent in ctx.
func searchSkills(ctx context.Context, terms []string) []fw.MemoryHit {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return nil
	}
	var hits []fw.MemoryHit
	for _, sk := range a.Skills() {
		body, err := sk.Body()
		if err != nil {
			continue
		}
		score, snippet := matchScore(terms, sk.Name, sk.Description, body)
		if score == 0 {
			continue
		}
		hits = append(hits, fw.MemoryHit{
			Record: fw.MemoryRecord{
				Ref:         "skill://" + sk.Name,
				Kind:        fw.MemoryKindSkill,
				Title:       sk.Name,
				Description: sk.Description,
				Source:      "skill",
				Trust:       "untrusted",
			},
			Score:   score,
			Snippet: snippet,
			Why:     "skill",
		})
	}
	return hits
}

func getRun(ctx context.Context, scope fw.MemoryScope, id string) (fw.MemoryRecord, error) {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.MemoryRecord{}, fw.ErrMemoryUnsupported
	}
	r, err := a.Get(ctx, id)
	if err != nil || !runVisible(scope, r) {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s\nAgent: %s\nState: %s\n", r.ID, r.Agent, r.State)
	if r.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", r.Title)
	}
	if r.Input != "" {
		fmt.Fprintf(&b, "\nInput:\n%s\n", r.Input)
	}
	if r.Output != "" {
		fmt.Fprintf(&b, "\nOutput:\n%s\n", r.Output)
	}
	if r.Error != "" {
		fmt.Fprintf(&b, "\nError:\n%s\n", r.Error)
	}
	return fw.MemoryRecord{
		Ref:     "run://" + r.ID,
		Kind:    fw.MemoryKindRun,
		Scope:   scope,
		Title:   r.Title,
		Body:    b.String(),
		Source:  "ledger",
		Trust:   "untrusted",
		RunID:   r.ID,
		Updated: r.UpdatedAt,
	}, nil
}

func getFile(ctx context.Context, name string) (fw.MemoryRecord, error) {
	work := fw.FilesFrom(ctx)
	if work == nil {
		return fw.MemoryRecord{}, fw.ErrMemoryUnsupported
	}
	if !fs.ValidPath(name) || name == "." {
		return fw.MemoryRecord{}, fmt.Errorf("the ref %q is not a file of the workspace", "file://"+name)
	}
	info, err := work.Stat(name)
	if err != nil || info.IsDir() {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	return fw.MemoryRecord{
		Ref:     "file://" + name,
		Kind:    fw.MemoryKindFile,
		Title:   name,
		Body:    readBounded(work, name, maxRead),
		Source:  "workspace",
		Trust:   "untrusted",
		Size:    info.Size(),
		Updated: info.ModTime(),
	}, nil
}

func getSkill(ctx context.Context, name string) (fw.MemoryRecord, error) {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.MemoryRecord{}, fw.ErrMemoryUnsupported
	}
	sk, ok := a.SkillNamed(name)
	if !ok {
		return fw.MemoryRecord{}, fw.ErrMemoryNotFound
	}
	body, err := sk.Body()
	if err != nil {
		return fw.MemoryRecord{}, err
	}
	return fw.MemoryRecord{
		Ref:         "skill://" + sk.Name,
		Kind:        fw.MemoryKindSkill,
		Title:       sk.Name,
		Description: sk.Description,
		Body:        body,
		Source:      "skill",
		Trust:       "untrusted",
	}, nil
}

type weightedField struct {
	text   string
	weight float64
}

func matchScore(terms []string, name, title, body string) (float64, string) {
	return fieldScore(terms,
		weightedField{text: name, weight: 6},
		weightedField{text: title, weight: 3},
		weightedField{text: body, weight: 1},
	)
}

// fieldScore adds the weight of each field that holds each term, and
// returns a snippet around the first match of the first field that has
// one.
func fieldScore(terms []string, fields ...weightedField) (float64, string) {
	score := 0.0
	for _, f := range fields {
		lower := strings.ToLower(f.text)
		for _, term := range terms {
			if strings.Contains(lower, term) {
				score += f.weight
			}
		}
	}
	if score == 0 {
		return 0, ""
	}
	for i := len(fields) - 1; i >= 0; i-- {
		if s := snippetFor(fields[i].text, terms); s != "" {
			return score, s
		}
	}
	return score, ""
}

func snippetFor(text string, terms []string) string {
	lower := strings.ToLower(text)
	at := -1
	for _, term := range terms {
		if i := strings.Index(lower, term); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return ""
	}
	start := max(at-snippetLen/3, 0)
	end := min(start+snippetLen, len(text))
	for start > 0 && !utf8RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8RuneStart(text[end]) {
		end++
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "..." + s
	}
	if end < len(text) {
		s += "..."
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

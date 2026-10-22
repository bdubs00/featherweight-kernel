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
	// MaxSearchBytes bounds the bytes that one search reads from the files
	// of the workspace, and apart from them the bytes that a scan reads
	// from the notes. A search that reaches it stops that kind and says so
	// in a note of its result.
	MaxSearchBytes = 16 << 20
	snippetLen     = 240
)

// noteIndex is the optional index of the notes. The build without the
// memory_sqlite tag has no index and scans the notes.
type noteIndex interface {
	Search(ctx context.Context, scope fw.MemoryScope, notesDir, indexDir string, terms []string, limit int) ([]fw.MemoryHit, error)
	// Remove removes the note name, and every note under name when it is
	// a directory, from the index in indexDir, when that index exists.
	Remove(ctx context.Context, indexDir, name string) error
	Close() error
}

// RecallResult is the answer of Recall: the hits, best first, and notes
// for the reader, such as a bound that stopped the search of one kind
// before it read everything.
type RecallResult struct {
	Hits  []fw.MemoryHit `json:"hits"`
	Notes []string       `json:"notes,omitempty"`
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

// Search implements fw.Memory. It is Recall without the notes of the
// result.
func (s *Store) Search(ctx context.Context, scope fw.MemoryScope, q fw.MemoryQuery) ([]fw.MemoryHit, error) {
	res, err := s.Recall(ctx, scope, q)
	return res.Hits, err
}

// Recall searches the notes of the scope, the runs of its agent that its
// principal started, the workspace of the run in ctx, and the skills that
// the profile of the run allows. The runs, the files, and the skills need
// the agent in ctx (fw.AgentFrom); without it, only notes match. The
// result says in its notes when a bound stopped a search early.
func (s *Store) Recall(ctx context.Context, scope fw.MemoryScope, q fw.MemoryQuery) (RecallResult, error) {
	terms := strings.Fields(strings.ToLower(q.Text))
	if len(terms) == 0 {
		return RecallResult{}, errors.New("a recall needs words to find")
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
	var notes []string
	add := func(h []fw.MemoryHit, note string) {
		hits = append(hits, h...)
		if note != "" {
			notes = append(notes, note)
		}
	}
	for _, k := range kinds {
		switch k {
		case fw.MemoryKindNote:
			add(s.searchNotes(ctx, scope, terms))
		case fw.MemoryKindRun:
			add(searchRuns(ctx, scope, terms), "")
		case fw.MemoryKindFile:
			add(searchFiles(ctx, terms))
		case fw.MemoryKindSkill:
			add(searchSkills(ctx, scope, terms), "")
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
	return RecallResult{Hits: hits, Notes: notes}, nil
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
		return getSkill(ctx, scope, strings.TrimPrefix(ref, "skill://"))
	}
	return fw.MemoryRecord{}, fmt.Errorf("the ref %q must start with mem://, run://, file://, or skill://", ref)
}

// searchNotes searches the notes of the scope: through the index when the
// build has one, else by a scan that reads at most MaxSearchBytes. It
// returns a note for the reader when a bound stopped the scan.
func (s *Store) searchNotes(ctx context.Context, scope fw.MemoryScope, terms []string) ([]fw.MemoryHit, string) {
	dir, err := s.Dir(scope)
	if err != nil {
		return nil, ""
	}
	if s.index != nil {
		if idx, err := s.indexDir(scope); err == nil {
			if hits, err := s.index.Search(ctx, scope, dir, idx, terms, MaxLimit); err == nil {
				return hits, ""
			}
		}
	}
	notes, cut, err := walkNotes(ctx, dir, -1)
	if err != nil || len(notes) == 0 {
		return nil, ""
	}
	files, err := fw.DirFiles(dir)
	if err != nil {
		return nil, ""
	}
	note := ""
	if cut {
		note = fmt.Sprintf("The search of the notes stopped after %d entries of the memory directory, so some notes were not searched.", maxWalk)
	}
	budget := int64(MaxSearchBytes)
	var hits []fw.MemoryHit
	for _, n := range notes {
		if budget < min(n.size, maxRead) {
			note = fmt.Sprintf("The search of the notes stopped after it read %d MB, so some notes were not searched.", MaxSearchBytes>>20)
			break
		}
		body := readBounded(files, n.name, maxRead)
		budget -= int64(len(body))
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
	return hits, note
}

// searchRuns searches the runs of the agent of the scope that the
// principal of the scope started (runVisible). It lists them with the
// principal in ctx, so the kernel also authorizes each run for that
// principal (runCtx). It reads the run projection, not every event, so a
// search stays bounded.
func searchRuns(ctx context.Context, scope fw.MemoryScope, terms []string) []fw.MemoryHit {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return nil
	}
	runs, err := a.List(runCtx(ctx, scope))
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

// runVisible reports whether a run belongs to the agent of the scope and
// to the principal of the scope. An empty principal equals only an empty
// principal: a run that nobody started, such as a local run or a job of a
// publisher that names nobody, sees only such runs, and a run of a person
// never sees them.
func runVisible(scope fw.MemoryScope, r *fw.Run) bool {
	if r == nil {
		return false
	}
	agent := r.Agent
	if agent == "" {
		agent = "default"
	}
	return agent == scope.Agent && r.Principal == scope.Principal
}

// runCtx returns ctx with the principal of the scope in it, when ctx holds
// no principal, so the kernel authorizes List and Get of the runs for the
// person whose run reads them. A scope without a principal leaves ctx as
// it is.
func runCtx(ctx context.Context, scope fw.MemoryScope) context.Context {
	if fw.PrincipalFrom(ctx) != nil {
		return ctx
	}
	return fw.WithRunPrincipal(ctx, fw.RunInfo{Principal: scope.Principal})
}

// searchFiles searches the workspace of the run in ctx. It reads small text
// files for content and skips binaries. The walk visits at most maxFiles
// files and reads at most MaxSearchBytes; it returns a note for the reader
// when a bound stopped it.
func searchFiles(ctx context.Context, terms []string) ([]fw.MemoryHit, string) {
	work := fw.FilesFrom(ctx)
	if work == nil {
		return nil, ""
	}
	var hits []fw.MemoryHit
	note := ""
	budget := int64(MaxSearchBytes)
	seen := 0
	_ = fs.WalkDir(work, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			// The root is unreadable only when it has no entry. The workspace
			// also reports an entry it could not read, such as a link that
			// leaves the workspace, as an error on "." with the entries, and
			// that must not end the walk.
			if name == "." && d == nil {
				return err
			}
			if name == "." {
				return nil
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
			note = fmt.Sprintf("The search of the files stopped after %d files, so some files were not searched.", maxFiles)
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		body := ""
		if info.Size() <= maxRead {
			if budget < info.Size() {
				note = fmt.Sprintf("The search of the files stopped after it read %d MB, so some files were not searched.", MaxSearchBytes>>20)
				return fs.SkipAll
			}
			body = readBounded(work, name, maxRead)
			budget -= int64(len(body))
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
	return hits, note
}

// skillsOf returns the skills that the profile of the run of the scope
// allows, as the system prompt of the run lists them (fw.SkillsOfRun).
func skillsOf(a *fw.Agent, scope fw.MemoryScope) []fw.Skill {
	return a.SkillsOfRun(fw.RunInfo{RunID: scope.Task, Agent: scope.Agent})
}

// searchSkills searches the skills that the profile of the run allows.
func searchSkills(ctx context.Context, scope fw.MemoryScope, terms []string) []fw.MemoryHit {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return nil
	}
	var hits []fw.MemoryHit
	for _, sk := range skillsOf(a, scope) {
		// A skill of an MCP server is read at the moment of use, not
		// ahead of need, so a search matches its name and description.
		body := ""
		if sk.Origin == "" {
			var err error
			if body, err = sk.Body(); err != nil {
				continue
			}
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
	r, err := a.Get(runCtx(ctx, scope), id)
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
	// A hidden part names the state of fw, of a harness, or of a tool, such
	// as the ledger or the transcripts of other runs. The search skips those
	// paths, and a ref does not read them either.
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return fw.MemoryRecord{}, fmt.Errorf("the ref %q names a hidden path, which recall does not read", "file://"+name)
		}
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

func getSkill(ctx context.Context, scope fw.MemoryScope, name string) (fw.MemoryRecord, error) {
	a := fw.AgentFrom(ctx)
	if a == nil {
		return fw.MemoryRecord{}, fw.ErrMemoryUnsupported
	}
	var sk fw.Skill
	ok := false
	for _, s := range skillsOf(a, scope) {
		if s.Name == name {
			sk, ok = s, true
			break
		}
	}
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

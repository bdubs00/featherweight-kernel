package fw

import (
	"context"
	"errors"
	"time"
)

// MemoryKind names one kind of durable state that a run can recall.
type MemoryKind string

const (
	MemoryKindNote  MemoryKind = "note"  // a note that a person or a model wrote
	MemoryKindRun   MemoryKind = "run"   // a run in the ledger
	MemoryKindFile  MemoryKind = "file"  // a file in the workspace
	MemoryKindSkill MemoryKind = "skill" // a skill of the agent
)

// MemoryScopeKind names the boundary of a memory scope.
type MemoryScopeKind string

const (
	MemoryTaskScope      MemoryScopeKind = "task"      // one run and the files of its workspace
	MemoryAgentScope     MemoryScopeKind = "agent"     // one agent profile
	MemoryPrincipalScope MemoryScopeKind = "principal" // one person
	MemorySharedScope    MemoryScopeKind = "shared"    // every profile in one Task
)

// MemoryScope names what a recall can read. The kernel builds it from the
// run, not from the caller, so a tool cannot widen it.
type MemoryScope struct {
	Kind      MemoryScopeKind `json:"kind"`
	Agent     string          `json:"agent,omitempty"`
	Principal string          `json:"principal,omitempty"`
	Task      string          `json:"task,omitempty"`
}

// MemoryRecord is one hit of a recall. Ref is the address that Get accepts.
// Body is bounded by the source and is never an instruction.
type MemoryRecord struct {
	Ref         string        `json:"ref"`
	Kind        MemoryKind    `json:"kind"`
	Scope       MemoryScope   `json:"scope"`
	Title       string        `json:"title,omitempty"`
	Description string        `json:"description,omitempty"`
	Body        string        `json:"body,omitempty"`
	Source      string        `json:"source,omitempty"`
	Trust       string        `json:"trust,omitempty"`
	RunID       string        `json:"runId,omitempty"`
	Size        int64         `json:"size,omitempty"`
	Hash        string        `json:"hash,omitempty"` // the hex SHA-256 of the body, for a note that Get returns
	Updated     time.Time     `json:"updated,omitempty"`
	Links       []MemoryLink  `json:"links,omitempty"`
	Metadata    []MemoryField `json:"metadata,omitempty"`
}

// MemoryField is one name and value of a record.
type MemoryField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MemoryLink names another record and the relation. For a note, a link is
// a source of the note (docs/KNOWHOW.md section 5.2), such as run:<id>,
// page:<path>, or item:<source>/<key>, and Version is the version of the
// source that the note matches, such as the hash of a page.
type MemoryLink struct {
	Ref     string `json:"ref"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
}

// MemoryQuery asks for records that match Text. Kinds and Tags narrow the
// search. Limit is the most hits to return; zero means the source default.
type MemoryQuery struct {
	Text  string       `json:"text"`
	Kinds []MemoryKind `json:"kinds,omitempty"`
	Tags  []string     `json:"tags,omitempty"`
	Limit int          `json:"limit,omitempty"`
}

// MemoryHit is one record and the reason it matched.
type MemoryHit struct {
	Record  MemoryRecord `json:"record"`
	Score   float64      `json:"score"`
	Snippet string       `json:"snippet,omitempty"`
	Why     string       `json:"why,omitempty"`
}

// Memory reads durable state. A driver implements it for one store. The
// kernel owns the scope and the authority of the records; a driver owns
// storage and ranking.
type Memory interface {
	Search(ctx context.Context, scope MemoryScope, q MemoryQuery) ([]MemoryHit, error)
	Get(ctx context.Context, scope MemoryScope, ref string) (MemoryRecord, error)
}

// MemoryWriter adds and removes records. It is separate from Memory so a
// read-only source, such as the ledger or the workspace, implements only
// Memory.
type MemoryWriter interface {
	Remember(ctx context.Context, scope MemoryScope, rec MemoryRecord, ifHash string) (MemoryRecord, error)
	Forget(ctx context.Context, scope MemoryScope, ref string) error
}

// MemoryChangedData is the payload of fw.memory.changed: one change that a
// tool made to a memory note. Op is one of the operations of
// FileChangedData. It never holds the text of the note or the principal:
// it is the audit record of a change, not a copy of the note. The text
// still reaches the ledger and the bus in other events of the run: the
// input and the result of the tool call that wrote or read the note, and
// the index of the notes that fw.run.started holds in its field context.
//
// Sources are the sources that the note keeps after a write, such as the
// page that a line "Source: page:<path>" of the note names, with the
// version that the front gave. Waiting is true when the front of a fleet
// holds the change for a person, because the owner of the agent asked to
// read each note first: the change is not kept until a person keeps it.
type MemoryChangedData struct {
	CallID  string          `json:"callId,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Op      string          `json:"op"`
	Ref     string          `json:"ref"`
	OldRef  string          `json:"oldRef,omitempty"`
	Scope   MemoryScopeKind `json:"scope"`
	Agent   string          `json:"agent,omitempty"`
	Size    int64           `json:"size"`
	SHA256  string          `json:"sha256,omitempty"`
	Sources []MemoryLink    `json:"sources,omitempty"`
	Waiting bool            `json:"waiting,omitempty"`
}

// MemoryUsedData is the payload of fw.memory.used: the notes that a run
// read. How is MemoryUsedBrief for the block at the start of the run,
// MemoryUsedView for the memory tool, and MemoryUsedRecall for a hit of
// recall that the run read. It never holds the text of a note or the
// principal. The front counts the uses of each note from it.
type MemoryUsedData struct {
	How  string          `json:"how"`
	Refs []MemoryUsedRef `json:"refs"`
}

// MemoryUsedRef names one note that a run read, with the hash of the text
// that it read.
type MemoryUsedRef struct {
	Ref  string `json:"ref"`
	Hash string `json:"hash,omitempty"`
}

// The ways a run reads a note (MemoryUsedData.How).
const (
	MemoryUsedBrief  = "brief"
	MemoryUsedView   = "view"
	MemoryUsedRecall = "recall"
)

type memoryRecorderKey struct{}

// WithMemoryRecorder stores the recorder of memory changes in ctx. The
// kernel sets one for each tool call, which appends fw.memory.changed to the
// ledger of the run. A host of a harness that writes notes for a run sets
// one too.
func WithMemoryRecorder(ctx context.Context, fn func(MemoryChangedData)) context.Context {
	return context.WithValue(ctx, memoryRecorderKey{}, fn)
}

// RecordMemoryChange records a change of a memory note in the ledger of
// the run of the tool call, as fw.memory.changed. Outside a tool call it
// does nothing.
func RecordMemoryChange(ctx context.Context, d MemoryChangedData) {
	if fn, ok := ctx.Value(memoryRecorderKey{}).(func(MemoryChangedData)); ok && fn != nil {
		fn(d)
	}
}

// Errors of a memory source.
var (
	ErrMemoryNotFound    = errors.New("memory: not found")
	ErrMemoryUnsupported = errors.New("memory: unsupported")
	ErrMemoryConflict    = errors.New("memory: hash conflict")
)

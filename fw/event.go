package fw

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SpecVersion is the CloudEvents version of every ledger event.
const SpecVersion = "1.0"

// Event is one ledger entry in the CloudEvents 1.0 shape.
type Event struct {
	SpecVersion string    `json:"specversion"`
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Type        string    `json:"type"`
	Time        time.Time `json:"time"`
	Subject     string    `json:"subject"` // the run id
	Seq         uint64    `json:"seq"`
	// Node names the process that appended the event (Config.Node), so a
	// consumer of many processes, such as the front of a fleet, knows which
	// one holds the run.
	Node        string          `json:"node,omitempty"`
	TraceParent string          `json:"traceparent,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

// Event types. The data shape of each is the struct with the matching name.
const (
	EvRunQueued         = "fw.run.queued"
	EvRunStarted        = "fw.run.started"
	EvMessageReceived   = "fw.message.received"
	EvTurnStarted       = "fw.turn.started"
	EvModelRequested    = "fw.model.requested"
	EvModelResponded    = "fw.model.responded"
	EvToolRequested     = "fw.tool.requested"
	EvToolResponded     = "fw.tool.responded"
	EvToolProgress      = "fw.tool.progress"
	EvFileChanged       = "fw.file.changed"
	EvApprovalRequested = "fw.approval.requested"
	EvApprovalDecided   = "fw.approval.decided"
	EvContextCompacted  = "fw.context.compacted"
	EvBudgetExceeded    = "fw.budget.exceeded"
	EvError             = "fw.error"
	EvTurnFinished      = "fw.turn.finished"
	EvRunSuspended      = "fw.run.suspended"
	EvRunResumed        = "fw.run.resumed"
	EvRunFinished       = "fw.run.finished"
	EvRunChild          = "fw.run.child"      // a spawner started a child of this run in another process; see RunChildData
	EvChildReported     = "fw.child.reported" // the usage that a child in another process last reported; see ChildReportedData
	EvLoopDetected      = "fw.loop.detected"
	// EvToolSend is published on the bus by the send tool. It is not a ledger event.
	EvToolSend = "fw.tool.send"
)

// IsTerminal reports whether an event type ends a run.
func IsTerminal(typ string) bool { return typ == EvRunFinished }

// LeavesFinished reports whether an event of the type leaves a finished run
// finished: fw.run.annotated, fw.run.child, and fw.child.reported add facts
// to a run and do no work on it, so they can arrive after its end, for
// example the report of a child that ends after its parent, or a branch of
// a finished run in another process. Every other event after the end, such
// as a follow-up message, works on the run again.
func LeavesFinished(typ string) bool {
	return typ == EvRunAnnotated || typ == EvRunChild || typ == EvChildReported
}

// NewEvent builds an event for a run. Seq and Source are set by the ledger.
func NewEvent(runID, typ string, data any) (Event, error) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return Event{}, fmt.Errorf("fw: marshal %s data: %w", typ, err)
		}
		raw = b
	}
	return Event{
		SpecVersion: SpecVersion,
		ID:          NewID(),
		Type:        typ,
		Time:        time.Now().UTC(),
		Subject:     runID,
		Data:        raw,
	}, nil
}

// MustEvent is NewEvent for data that always marshals.
func MustEvent(runID, typ string, data any) Event {
	e, err := NewEvent(runID, typ, data)
	if err != nil {
		panic(err)
	}
	return e
}

// Decode unmarshals the event data into v.
func (e Event) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// Origins of work.
const (
	OriginHTTP = "http"
	OriginA2A  = "a2a"
	OriginMCP  = "mcp"
	OriginBus  = "bus"
	OriginCLI  = "cli"
	OriginTool = "tool"
)

// Run outcomes.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
	OutcomeCanceled  = "canceled"
)

// RunQueuedData is the payload of fw.run.queued. Instructions and Profile
// come from the request (see RunRequest). Depth is 0 for a top-level run
// and one more than the depth of the parent for a child run.
type RunQueuedData struct {
	Agent        string            `json:"agent"`
	Input        string            `json:"input"`
	Instructions string            `json:"instructions,omitempty"`
	Profile      *AgentProfile     `json:"profile,omitempty"`
	Budget       *Budget           `json:"budget,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Origin       string            `json:"origin"`
	Principal    string            `json:"principal,omitempty"`
	// PrincipalKind is the kind of the principal, such as user, when the
	// caller that started the run is the principal.
	PrincipalKind string  `json:"principalKind,omitempty"`
	Parent        string  `json:"parent,omitempty"`
	Depth         int     `json:"depth,omitempty"`
	Files         []Block `json:"files,omitempty"` // file blocks that reference the attachments
	// Kept is set on the branch of a run (Agent.Fork): the number of events
	// at the start of the ledger of the branch that it kept from its
	// source, this event included. The usage of the branch leaves them out,
	// because the source holds that usage (see Run.Apply). The process that
	// writes the branch sets it to the number of the copies, also for a
	// branch that another process sends; a request cannot set it.
	Kept uint64 `json:"kept,omitempty"`
	// ExtraTools names the tools that the inline profile gives the run
	// beyond the tool list of its named profile, so every surface can tell
	// the person.
	ExtraTools []string `json:"extraTools,omitempty"`
	// Resolved is the configuration of the run that the kernel resolved
	// once, when it queued the run (see ResolvedProfile). A ledger of an
	// older kernel has none, and such a run reads its named profile at each
	// step.
	Resolved *ResolvedProfile `json:"resolved,omitempty"`
}

// ResolvedProfile is the configuration of a run that the kernel resolves
// from the named profile, the inline profile of the request, and the
// configuration when it queues the run. The run keeps it for its life:
// across turns, follow-ups, waits, restarts, and branches, so a change to
// the profile or to the configuration reaches new runs only.
//
// It holds no configured text. The system prompt and the instructions stay
// in the configuration, the text of the inline profile is in
// RunQueuedData.Profile, and the kernel keeps the text of the run in
// memory. SystemDigest and InstructionsDigest tie the run to that text:
// each is "sha256:" and the hex SHA-256 of the system prompt of the run
// (the inline one, else the named one, else DefaultSystemPrompt) and of
// its instructions (those of the named profile, then those of the inline
// profile, each without the spaces at its start and end, the empty ones
// left out, joined by a blank line), as the model reads them. After a
// restart, a text with other digests means that the configuration
// changed: the run goes on with the text now, and the kernel records the
// change on the run (MetaDriftSystem, MetaDriftInstructions).
//
// Model is a selector. Effort and TimeZone are those of the overlay, else
// the effort of the configuration and the zone of the parent run or of the
// configuration. Skills and Tools are the allowlists of the overlay.
// Budget is the budget of the overlay with each zero field taken from the
// budget of the configuration. Policy is the rules of the overlay; the
// rules that the named profile has now apply on top as a tightening. The
// description and the access block of a profile are never part of it.
type ResolvedProfile struct {
	Model              string   `json:"model,omitempty"`
	Effort             string   `json:"effort,omitempty"`
	Skills             []string `json:"skills"` // nil means every skill, and an empty list none
	Tools              []string `json:"tools,omitempty"`
	TimeZone           string   `json:"timeZone,omitempty"`
	Policy             *Rules   `json:"policy,omitempty"`
	Budget             *Budget  `json:"budget,omitempty"`
	SystemDigest       string   `json:"systemDigest,omitempty"`
	InstructionsDigest string   `json:"instructionsDigest,omitempty"`
}

// Metadata keys that the kernel sets with fw.run.annotated when the text
// of a run differs after a restart from the text that it was queued with
// (see ResolvedProfile). The value is "<old digest> -> <new digest>".
const (
	MetaDriftSystem       = "fw.drift.system"
	MetaDriftInstructions = "fw.drift.instructions"
)

// RunStartedData is the payload of fw.run.started.
type RunStartedData struct {
	Model string   `json:"model"`
	Tools []string `json:"tools"`
	Brief string   `json:"brief,omitempty"` // the sections that tools add to the system prompt (see Briefer)
	// TimeZone is the zone of the run for its life: the clock tool and the
	// time notes read and write times in it (see LoadZone).
	TimeZone string `json:"timeZone,omitempty"`
}

// MessageReceivedData is the payload of fw.message.received.
type MessageReceivedData struct {
	Role    Role    `json:"role"`
	Content []Block `json:"content"`
	Origin  string  `json:"origin"`
	// Note names the kind of a system message from the kernel: NoteLoop or
	// NoteTime. It is empty for every other message.
	Note string `json:"note,omitempty"`
}

// Kinds of the system messages from the kernel.
const (
	NoteLoop = "loop" // the loop guard warns that the model repeats itself
	NoteTime = "time" // a time note gives the time after a gap
)

// TurnData is the payload of fw.turn.started and fw.turn.finished.
type TurnData struct {
	Turn int `json:"turn"`
}

// ModelRequestedData is the payload of fw.model.requested.
type ModelRequestedData struct {
	Provider             string `json:"provider"`
	Model                string `json:"model"`
	Effort               string `json:"effort,omitempty"`
	EstimatedInputTokens int    `json:"estimatedInputTokens,omitempty"`
}

// ModelRespondedData is the payload of fw.model.responded.
type ModelRespondedData struct {
	Content    []Block         `json:"content"`
	Raw        json.RawMessage `json:"raw,omitempty"` // provider blocks, unchanged
	StopReason StopReason      `json:"stopReason"`
	StopDetail string          `json:"stopDetail,omitempty"`
	Usage      Usage           `json:"usage"`
	LatencyMS  int64           `json:"latencyMs"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
}

// ToolRequestedData is the payload of fw.tool.requested.
type ToolRequestedData struct {
	Tool   string          `json:"tool"`
	CallID string          `json:"callId"`
	Input  json.RawMessage `json:"input"`
	TaskID string          `json:"taskId,omitempty"` // MCP task handle, when the tool returned one
	// Idempotent records ToolSpec.Idempotent at invocation time. False,
	// including a missing field in an older event, requires the file ledger
	// to fsync the intent before Append succeeds. Recovery still consults
	// the current tool spec; this field does not pin the tool implementation
	// or guarantee exactly-once effects.
	Idempotent bool `json:"idempotent,omitempty"`
}

// ToolRespondedData is the payload of fw.tool.responded.
type ToolRespondedData struct {
	CallID    string  `json:"callId"`
	Tool      string  `json:"tool,omitempty"`
	Content   []Block `json:"content"`
	IsError   bool    `json:"isError"`
	LatencyMS int64   `json:"latencyMs"`
}

// ToolProgressData is the payload of fw.tool.progress.
type ToolProgressData struct {
	CallID   string  `json:"callId"`
	Progress float64 `json:"progress"`
	Total    float64 `json:"total,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// ApprovalRequestedData is the payload of fw.approval.requested.
type ApprovalRequestedData struct {
	ApprovalID   string          `json:"approvalId"`
	Tool         string          `json:"tool"`
	CallID       string          `json:"callId,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Schema       json.RawMessage `json:"schema,omitempty"`       // form schema when a tool asked a question
	RequestState string          `json:"requestState,omitempty"` // opaque MCP state to echo back
	// Connect is set when the call waits for the person to connect a
	// service (see ConnectRequest). Reason then holds its Message.
	Connect *ConnectRequest `json:"connect,omitempty"`
}

// ApprovalDecidedData is the payload of fw.approval.decided.
type ApprovalDecidedData struct {
	ApprovalID string          `json:"approvalId"`
	Decision   string          `json:"decision"` // allow or deny
	Decider    string          `json:"decider,omitempty"`
	Note       string          `json:"note,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"` // form answers when a tool asked a question
}

// ContextCompactedData is the payload of fw.context.compacted.
type ContextCompactedData struct {
	Epoch   int       `json:"epoch"`
	Summary Block     `json:"summary"`
	Kept    []Message `json:"kept,omitempty"` // the messages after the summary in the new epoch
	// Usage counts the tokens of the summary calls, so the usage, the cost,
	// and the budget of the run include them. Provider and Model name the
	// model of the last summary call, for the price.
	Usage    *Usage `json:"usage,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// LoopDetectedData is the payload of fw.loop.detected. The kernel appends
// it when the model repeats the same tool call, or the same cycle of two or
// three calls. Count is the number of repeats in a row, and Period is the
// length of the cycle. Action is "warned" when the kernel told the model
// that it repeats itself, and "failed" when the run ended for it.
type LoopDetectedData struct {
	Tool   string `json:"tool"`
	Count  int    `json:"count"`
	Period int    `json:"period"`
	Action string `json:"action"`
}

// BudgetExceededData is the payload of fw.budget.exceeded. Limit names the
// limit, such as maxTokens, and Value is the usage that reached it. Max is
// the limit itself, when the kernel recorded it. Scope is ScopeTree when
// the usage of a run and of the runs under it reached a limit that holds
// for the tree (see Budget), and Run then names the run whose budget holds
// the limit: this run or one of its ancestors.
type BudgetExceededData struct {
	Limit string  `json:"limit"`
	Value float64 `json:"value"`
	Max   float64 `json:"max,omitempty"`
	Scope string  `json:"scope,omitempty"`
	Run   string  `json:"run,omitempty"`
}

// ScopeTree is the scope of a fw.budget.exceeded event that a tree of runs
// reached together.
const ScopeTree = "tree"

// ChildReportedData is the payload of EvChildReported. The kernel appends
// it to a parent run when it reads a child in another process and sees
// that the child ended, and at most once a minute while the usage of the
// child grows, so a restart of the parent knows what the tree of the child
// used. TreeUsage is the usage of the child and of every run under it, as
// the child reported it (Run.TreeUsage), State is the state of the child,
// and Open is its Run.TreeOpen.
type ChildReportedData struct {
	Child     string   `json:"child"`
	Member    string   `json:"member,omitempty"`
	State     RunState `json:"state"`
	TreeUsage RunUsage `json:"treeUsage"`
	Open      int      `json:"open,omitempty"`
}

// ErrorData is the payload of fw.error.
type ErrorData struct {
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// RunSuspendedData is the payload of fw.run.suspended.
type RunSuspendedData struct {
	Reason string `json:"reason"`
	// Totals is the usage of the run before this event, as for
	// RunFinishedData.
	Totals *RunUsage `json:"totals,omitempty"`
}

// RunResumedData is the payload of fw.run.resumed.
type RunResumedData struct {
	FromSeq uint64 `json:"fromSeq"`
}

// RunFinishedData is the payload of fw.run.finished.
type RunFinishedData struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
	// Totals is the usage of the run before this event, as the kernel knows
	// it: the sum over every earlier event of the run, across follow-ups,
	// so a run that finishes again carries more. The kernel sets it on
	// every fw.run.finished and fw.run.suspended that it appends. The
	// projection takes the totals in place of its own sum (see Run.Apply),
	// so a consumer that receives only the lifecycle events of a run, such
	// as the front of a fleet, knows the usage of the run without
	// fw.model.responded. CostUSD is the cost at the prices of the kernel
	// that ran the calls, and UnpricedTokens counts the tokens that it had
	// no price for. An event of an older kernel has no totals.
	Totals *RunUsage `json:"totals,omitempty"`
}

// IDTime returns the time that an id from NewID holds, to the
// millisecond. It reports false for a string that is not such an id.
func IDTime(id string) (time.Time, bool) {
	if len(id) != 26 {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		v := strings.IndexByte(crockford, id[i])
		if v < 0 || (i == 0 && v > 7) {
			return time.Time{}, false
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

// NewID returns a ULID: 48 bits of milliseconds and 80 random bits, in
// Crockford base32, so ids sort by time.
func NewID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err)
	}
	return encodeBase32(b)
}

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func encodeBase32(b [16]byte) string {
	var out [26]byte
	out[0] = crockford[(b[0]&224)>>5]
	out[1] = crockford[b[0]&31]
	out[2] = crockford[(b[1]&248)>>3]
	out[3] = crockford[((b[1]&7)<<2)|((b[2]&192)>>6)]
	out[4] = crockford[(b[2]&62)>>1]
	out[5] = crockford[((b[2]&1)<<4)|((b[3]&240)>>4)]
	out[6] = crockford[((b[3]&15)<<1)|((b[4]&128)>>7)]
	out[7] = crockford[(b[4]&124)>>2]
	out[8] = crockford[((b[4]&3)<<3)|((b[5]&224)>>5)]
	out[9] = crockford[b[5]&31]
	out[10] = crockford[(b[6]&248)>>3]
	out[11] = crockford[((b[6]&7)<<2)|((b[7]&192)>>6)]
	out[12] = crockford[(b[7]&62)>>1]
	out[13] = crockford[((b[7]&1)<<4)|((b[8]&240)>>4)]
	out[14] = crockford[((b[8]&15)<<1)|((b[9]&128)>>7)]
	out[15] = crockford[(b[9]&124)>>2]
	out[16] = crockford[((b[9]&3)<<3)|((b[10]&224)>>5)]
	out[17] = crockford[b[10]&31]
	out[18] = crockford[(b[11]&248)>>3]
	out[19] = crockford[((b[11]&7)<<2)|((b[12]&192)>>6)]
	out[20] = crockford[(b[12]&62)>>1]
	out[21] = crockford[((b[12]&1)<<4)|((b[13]&240)>>4)]
	out[22] = crockford[((b[13]&15)<<1)|((b[14]&128)>>7)]
	out[23] = crockford[(b[14]&124)>>2]
	out[24] = crockford[((b[14]&3)<<3)|((b[15]&224)>>5)]
	out[25] = crockford[b[15]&31]
	return string(out[:])
}

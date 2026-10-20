package fw

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RunState is the one-word state of a run.
type RunState string

const (
	StateQueued        RunState = "queued"
	StateRunning       RunState = "running"
	StateInputRequired RunState = "input-required"
	StateCompleted     RunState = "completed"
	StateFailed        RunState = "failed"
	StateCanceled      RunState = "canceled"
	StateSuspended     RunState = "suspended"
)

// Terminal reports whether the state is final.
func (s RunState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCanceled
}

// Budget limits one run. A zero value means no limit for that field.
//
// The first four limits bound the size of a run. The last three bound the
// shape of a run and stop a model that loops: MaxToolCalls caps the tool
// calls of the whole run, MaxCallsPerTurn caps the tool calls of one model
// response, and MaxRepeats caps how many times in a row the model can make
// the same tool call with the same input. A repeat is also a cycle of two or
// three calls that comes back in the same order. One turn before MaxRepeats
// is reached, the kernel tells the model that it is repeating itself. At
// MaxRepeats, the run fails with fw.budget.exceeded.
//
// MaxTokens, MaxCostUSD, and MaxToolCalls hold for the tree of the run: the
// run and every run under it, in this process and in others, share them
// (docs/ARCHITECTURE.md section 5.2). MaxTurns, MaxCallsPerTurn, and
// MaxRepeats hold for each run alone, because they describe the shape of
// one conversation. The deadline counts the working time of the run and of
// each of its ancestors.
//
// The last three fields shape the final report (docs/ARCHITECTURE.md
// section 5.2). When a run reaches MaxTurns, MaxToolCalls, or the
// deadline and FinalReport is true, the kernel makes one more model call
// with the instruction to report what the run did, what is left, and where
// its work is. The text of that call becomes the output of the run, which
// still fails with the budget error. ReportMaxTokens caps the output of
// the call, and ReportTimeout bounds its time; zero takes
// DefaultReportMaxTokens and DefaultReportTimeout, so the call is always
// bounded. A run that reaches MaxTokens or MaxCostUSD gets no such call,
// because that call spends past a limit of money. A nil FinalReport
// means off; DefaultConfig sets it to true.
type Budget struct {
	MaxTurns        int           `yaml:"maxTurns,omitempty" json:"maxTurns,omitempty"`
	MaxTokens       int           `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`
	MaxCostUSD      float64       `yaml:"maxCostUSD,omitempty" json:"maxCostUSD,omitempty"`
	Deadline        time.Duration `yaml:"deadline,omitempty" json:"deadline,omitempty"`
	MaxToolCalls    int           `yaml:"maxToolCalls,omitempty" json:"maxToolCalls,omitempty"`
	MaxCallsPerTurn int           `yaml:"maxCallsPerTurn,omitempty" json:"maxCallsPerTurn,omitempty"`
	MaxRepeats      int           `yaml:"maxRepeats,omitempty" json:"maxRepeats,omitempty"`
	FinalReport     *bool         `yaml:"finalReport,omitempty" json:"finalReport,omitempty"`
	ReportMaxTokens int           `yaml:"reportMaxTokens,omitempty" json:"reportMaxTokens,omitempty"`
	ReportTimeout   time.Duration `yaml:"reportTimeout,omitempty" json:"reportTimeout,omitempty"`
}

// Defaults of the final report call (see Budget).
const (
	DefaultReportMaxTokens = 1024
	DefaultReportTimeout   = 2 * time.Minute
)

// reportOn reports whether the final report is on.
func (b Budget) reportOn() bool { return b.FinalReport != nil && *b.FinalReport }

// reportTokens returns the output cap of the final report call.
func (b Budget) reportTokens() int {
	if b.ReportMaxTokens > 0 {
		return b.ReportMaxTokens
	}
	return DefaultReportMaxTokens
}

// reportTimeout returns the time limit of the final report call.
func (b Budget) reportTimeout() time.Duration {
	if b.ReportTimeout > 0 {
		return b.ReportTimeout
	}
	return DefaultReportTimeout
}

// negative reports whether a field of the budget is below zero.
func (b Budget) negative() bool {
	return b.MaxTurns < 0 || b.MaxTokens < 0 || b.MaxCostUSD < 0 || b.Deadline < 0 || b.MaxToolCalls < 0 ||
		b.MaxCallsPerTurn < 0 || b.MaxRepeats < 0 || b.ReportMaxTokens < 0 || b.ReportTimeout < 0
}

// Merge returns b with every zero field taken from o.
func (b Budget) Merge(o Budget) Budget {
	if b.MaxTurns == 0 {
		b.MaxTurns = o.MaxTurns
	}
	if b.MaxTokens == 0 {
		b.MaxTokens = o.MaxTokens
	}
	if b.MaxCostUSD == 0 {
		b.MaxCostUSD = o.MaxCostUSD
	}
	if b.Deadline == 0 {
		b.Deadline = o.Deadline
	}
	if b.MaxToolCalls == 0 {
		b.MaxToolCalls = o.MaxToolCalls
	}
	if b.MaxCallsPerTurn == 0 {
		b.MaxCallsPerTurn = o.MaxCallsPerTurn
	}
	if b.MaxRepeats == 0 {
		b.MaxRepeats = o.MaxRepeats
	}
	if b.FinalReport == nil {
		b.FinalReport = o.FinalReport
	}
	if b.ReportMaxTokens == 0 {
		b.ReportMaxTokens = o.ReportMaxTokens
	}
	if b.ReportTimeout == 0 {
		b.ReportTimeout = o.ReportTimeout
	}
	return b
}

// Cap returns b with every limit that is larger than the same limit of o
// lowered to it. A limit that o leaves at zero stays as it is. The kernel
// caps the budget of a child run by the budget of its parent, and the
// budget of a child in another process by what the tree of the parent has
// left (see Budget). A final report that o turns off is off in the result
// too, so a child makes no report where its parent makes none.
func (b Budget) Cap(o Budget) Budget {
	capInt := func(v, limit int) int {
		if limit > 0 && (v == 0 || v > limit) {
			return limit
		}
		return v
	}
	b.MaxTurns = capInt(b.MaxTurns, o.MaxTurns)
	b.MaxTokens = capInt(b.MaxTokens, o.MaxTokens)
	b.MaxToolCalls = capInt(b.MaxToolCalls, o.MaxToolCalls)
	b.MaxCallsPerTurn = capInt(b.MaxCallsPerTurn, o.MaxCallsPerTurn)
	b.MaxRepeats = capInt(b.MaxRepeats, o.MaxRepeats)
	if o.MaxCostUSD > 0 && (b.MaxCostUSD == 0 || b.MaxCostUSD > o.MaxCostUSD) {
		b.MaxCostUSD = o.MaxCostUSD
	}
	if o.Deadline > 0 && (b.Deadline == 0 || b.Deadline > o.Deadline) {
		b.Deadline = o.Deadline
	}
	if o.FinalReport != nil && !*o.FinalReport {
		off := false
		b.FinalReport = &off
	}
	b.ReportMaxTokens = capInt(b.ReportMaxTokens, o.ReportMaxTokens)
	if o.ReportTimeout > 0 && (b.ReportTimeout == 0 || b.ReportTimeout > o.ReportTimeout) {
		b.ReportTimeout = o.ReportTimeout
	}
	return b
}

// empty reports whether the budget sets no field.
func (b Budget) empty() bool {
	return b.FinalReport == nil && b.MaxTurns == 0 && b.MaxTokens == 0 && b.MaxCostUSD == 0 && b.Deadline == 0 &&
		b.MaxToolCalls == 0 && b.MaxCallsPerTurn == 0 && b.MaxRepeats == 0 && b.ReportMaxTokens == 0 && b.ReportTimeout == 0
}

// AgentProfile is a named agent configuration.
//
// Access names the owners, the operators, and the viewers of the agent
// (see Access). A profile without it follows access.default of the
// configuration. An inline profile on a request never carries Access.
//
// System is the system prompt. Instructions are more texts that follow it,
// in order, so one profile composes a base prompt with house rules and task
// notes. In fw.yaml, System and each entry of Instructions is either the
// text itself or the path of a file that holds it. Skills lists the skills
// that the profile can use, as globs over skill names: a nil list means
// every skill, and an empty list means none. Tools lists the tools, as globs
// over tool names, and an empty list means every tool. TimeZone is the zone
// of the runs of the profile. Empty means the zone of the parent run for a
// child, else time.zone of the configuration. Outcome asks for a grade of
// the answer of each run of the profile that is not a child run (see
// Outcome).
type AgentProfile struct {
	Name         string   `yaml:"name,omitempty" json:"name"`
	Description  string   `yaml:"description,omitempty" json:"description,omitempty"`
	System       string   `yaml:"system,omitempty" json:"system,omitempty"`
	Instructions []string `yaml:"instructions,omitempty" json:"instructions,omitempty"`
	Skills       []string `yaml:"skills,omitempty" json:"skills"`
	Tools        []string `yaml:"tools,omitempty" json:"tools,omitempty"`
	Model        string   `yaml:"model,omitempty" json:"model,omitempty"` // selector or a name from Config.Models
	Effort       string   `yaml:"effort,omitempty" json:"effort,omitempty"`
	TimeZone     string   `yaml:"timeZone,omitempty" json:"timeZone,omitempty"` // an IANA name; empty means time.zone
	Policy       *Rules   `yaml:"policy,omitempty" json:"policy,omitempty"`
	Budget       *Budget  `yaml:"budget,omitempty" json:"budget,omitempty"`
	Access       *Access  `yaml:"access,omitempty" json:"access,omitempty"` // who owns and uses the agent
	Outcome      *Outcome `yaml:"outcome,omitempty" json:"outcome,omitempty"`
}

// Overlay returns p with the set fields of o applied on top. A set System,
// Model, Effort, TimeZone, or Description replaces the value of p.
// Instructions are appended. A non-nil Tools or Skills list replaces the
// list of p. The policy is the stricter of the two, so an overlay can
// tighten the rules but cannot loosen them. The budget takes every set field of o, and the
// kernel caps the budget of a child run by its parent afterwards. A set
// outcome block replaces the block of p as a whole.
func (p AgentProfile) Overlay(o AgentProfile) AgentProfile {
	if o.Name != "" {
		p.Name = o.Name
	}
	if o.Description != "" {
		p.Description = o.Description
	}
	if o.System != "" {
		p.System = o.System
	}
	if len(o.Instructions) > 0 {
		p.Instructions = append(append([]string(nil), p.Instructions...), o.Instructions...)
	}
	if o.Skills != nil {
		// An empty list stays an empty list: it means no skill, while nil
		// means every skill.
		p.Skills = append(make([]string, 0, len(o.Skills)), o.Skills...)
	}
	if o.Tools != nil {
		p.Tools = append(make([]string, 0, len(o.Tools)), o.Tools...)
	}
	if o.Model != "" {
		p.Model = o.Model
	}
	if o.Effort != "" {
		p.Effort = o.Effort
	}
	if o.TimeZone != "" {
		p.TimeZone = o.TimeZone
	}
	if o.Policy != nil {
		if p.Policy == nil {
			r := *o.Policy
			p.Policy = &r
		} else {
			r := MergeRules(*p.Policy, *o.Policy)
			p.Policy = &r
		}
	}
	if o.Budget != nil {
		if p.Budget == nil {
			b := *o.Budget
			p.Budget = &b
		} else {
			b := o.Budget.Merge(*p.Budget)
			p.Budget = &b
		}
	}
	if o.Outcome != nil {
		out := *o.Outcome
		p.Outcome = &out
	}
	// Access is never taken from an overlay: a request cannot grant itself
	// a role.
	return p
}

// Approval is a pending question to the caller.
type Approval struct {
	ID           string          `json:"id"`
	Tool         string          `json:"tool,omitempty"`
	CallID       string          `json:"callId,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Schema       json.RawMessage `json:"schema,omitempty"`
	RequestState string          `json:"-"`
	RequestedAt  time.Time       `json:"requestedAt"`
	// Connect is set when the call waits for the person to connect a
	// service. Only the end of the connection allows it.
	Connect *ConnectRequest `json:"connect,omitempty"`
}

// RunUsage is the usage of a run so far. Every field is a sum, so the
// usages of several runs add field by field.
type RunUsage struct {
	Usage
	Turns     int     `json:"turns"`
	ToolCalls int     `json:"toolCalls,omitempty"`
	CostUSD   float64 `json:"costUSD,omitempty"`
	// UnpricedTokens counts the tokens of the model calls whose model has
	// no price in the price table that computed CostUSD. CostUSD leaves
	// them out, so their cost is unknown, not zero. A price of zero is a
	// price. Without a price table, every token is unpriced.
	UnpricedTokens int `json:"unpricedTokens,omitempty"`
}

// plus returns the sum of two usages, field by field.
func (u RunUsage) plus(o RunUsage) RunUsage {
	u.Usage = u.Usage.Add(o.Usage)
	u.Turns += o.Turns
	u.ToolCalls += o.ToolCalls
	u.CostUSD += o.CostUSD
	u.UnpricedTokens += o.UnpricedTokens
	return u
}

// atLeast returns u with each field raised to the same field of o. Every
// field of a usage only grows, so two reads of one run give the later one.
func (u RunUsage) atLeast(o RunUsage) RunUsage {
	u.InputTokens = max(u.InputTokens, o.InputTokens)
	u.OutputTokens = max(u.OutputTokens, o.OutputTokens)
	u.CacheReadTokens = max(u.CacheReadTokens, o.CacheReadTokens)
	u.CacheWriteTokens = max(u.CacheWriteTokens, o.CacheWriteTokens)
	u.Turns = max(u.Turns, o.Turns)
	u.ToolCalls = max(u.ToolCalls, o.ToolCalls)
	u.CostUSD = max(u.CostUSD, o.CostUSD)
	u.UnpricedTokens = max(u.UnpricedTokens, o.UnpricedTokens)
	return u
}

// tokens returns the tokens that the budget counts: input and output.
func (u RunUsage) tokens() int { return u.InputTokens + u.OutputTokens }

// Run is the status object of a run. It is a projection of the ledger.
type Run struct {
	ID    string `json:"id"`
	Agent string `json:"agent"`
	// Title names the work in a few words, such as "Fix checkout retries".
	// It is the metadata value MetaTitle when the request sets it, and
	// else TitleFrom of the input.
	Title     string            `json:"title,omitempty"`
	State     RunState          `json:"state"`
	Input     string            `json:"input"`
	Output    string            `json:"output,omitempty"`
	Error     string            `json:"error,omitempty"`
	Pending   *Approval         `json:"pending,omitempty"`
	Usage     RunUsage          `json:"usage"`
	Seq       uint64            `json:"seq"`
	Origin    string            `json:"origin,omitempty"`
	Principal string            `json:"principal,omitempty"`
	Parent    string            `json:"parent,omitempty"`
	Depth     int               `json:"depth,omitempty"` // 0 for a top-level run, 1 for its child
	Metadata  map[string]string `json:"metadata,omitempty"`
	// ExtraTools names the tools that this run has beyond the tool list of
	// its agent, from an inline profile.
	ExtraTools []string  `json:"extraTools,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// Member names the process that holds the run when it is not this one.
	// The kernel never sets it, and the ledger never holds it: a linked
	// package that shows the runs of other processes, such as the fleet of
	// the workbench, sets it on the runs that it brings in.
	Member string `json:"member,omitempty"`
	// UsageSeq is the sequence number of the latest event that carried the
	// totals of the run, fw.run.finished or fw.run.suspended, or of a kept
	// event after which a branch was finished. Usage took
	// those totals and then added the usage of the later events that the
	// projection saw. Zero means that no event carried totals: the run did
	// not reach either event yet, or an older kernel wrote its ledger. A
	// consumer that receives only the lifecycle events of a run, such as
	// the front of a fleet, knows the usage of the run up to this event.
	UsageSeq uint64 `json:"usageSeq,omitempty"`
	// TreeUsage is the usage of the run and of every run under it, as the
	// kernel that holds the run knows it: its children in this process as
	// they are now, and its children in other processes as each one last
	// reported its own tree. It includes Usage. A sum over several runs
	// adds Usage and never TreeUsage, which counts the same calls again.
	// It is nil for a run that started no child. The ledger never holds
	// it: the kernel sets it on each run that it reports, so a parent in
	// another process reads the whole tree of a child in one read.
	TreeUsage *RunUsage `json:"treeUsage,omitempty"`
	// TreeOpen counts the runs under the run that have not finished, as
	// the kernel knows them, with TreeUsage, and TreeWorking the runs of
	// those that work: queued or running. A parent in another process goes
	// on reading a finished child while TreeWorking is not zero, because
	// the tree of the child still grows; a run that waits for a person
	// does not count, and nothing reads it in the background.
	TreeOpen    int `json:"treeOpen,omitempty"`
	TreeWorking int `json:"treeWorking,omitempty"`
	// Outcome is the latest grade of the answer of a run with an outcome,
	// from fw.outcome.evaluated (see Outcome). It is nil for a run without
	// a rubric.
	Outcome *RunOutcome `json:"outcome,omitempty"`
	// kept is RunQueuedData.Kept: the events of a branch that are copies of
	// the events of its source, whose usage the branch leaves out.
	kept uint64
	// keptTurn is true while a turn that the branch kept is open and no
	// model call of it followed.
	keptTurn bool
	// budget holds the set fields of every fw.budget.changed of the run,
	// a later change on top of an earlier one (see Agent.ChangeBudget).
	budget Budget
}

// RunRequest describes a run to start.
//
// Instructions is text for this run alone. It follows the system prompt of
// the profile. Profile is an inline profile that overlays the named profile
// of Agent, so a caller shapes an agent for one run without a configuration
// change: a system prompt, a tool subset, a model, a policy, or a budget.
// The kernel keeps both in fw.run.queued, so a resume and a replay see them.
// Outcome asks for a grade of the answer of the run, and replaces the
// outcome of the profile (see Outcome).
type RunRequest struct {
	ID             string            `json:"id,omitempty"` // set by the kernel when empty
	Agent          string            `json:"agent,omitempty"`
	Input          string            `json:"input"`
	Instructions   string            `json:"instructions,omitempty"`
	Profile        *AgentProfile     `json:"profile,omitempty"`
	Budget         *Budget           `json:"budget,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	Origin         string            `json:"origin,omitempty"`
	Principal      string            `json:"principal,omitempty"`
	PrincipalKind  string            `json:"principalKind,omitempty"` // the kind of Principal, when a trusted process starts the run for a person
	Parent         string            `json:"parent,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Files          []Block           `json:"files,omitempty"` // attachments of the first message; see Attachment
	Outcome        *Outcome          `json:"outcome,omitempty"`
	// FilesRoot keeps the tools of the run in the root of the files tool
	// when tools.files.perRun gives every other run a directory of its
	// own, as the bootstrap of an ax workspace needs. Only a Go caller sets
	// it (InFilesRoot); no surface does.
	FilesRoot bool `json:"-"`
}

// RunOption changes a RunRequest.
type RunOption func(*RunRequest)

// AsAgent selects the profile.
func AsAgent(name string) RunOption { return func(r *RunRequest) { r.Agent = name } }

// WithRunBudget sets the budget of the run.
func WithRunBudget(b Budget) RunOption { return func(r *RunRequest) { r.Budget = &b } }

// WithInstructions adds text that follows the system prompt for this run.
func WithInstructions(s string) RunOption { return func(r *RunRequest) { r.Instructions = s } }

// WithRunProfile overlays an inline profile on the named profile of the run.
func WithRunProfile(p AgentProfile) RunOption { return func(r *RunRequest) { r.Profile = &p } }

// WithOrigin records where the work came from.
func WithOrigin(origin string) RunOption { return func(r *RunRequest) { r.Origin = origin } }

// WithPrincipalName records who asked.
func WithPrincipalName(p string) RunOption { return func(r *RunRequest) { r.Principal = p } }

// WithParent records the parent run.
func WithParent(id string) RunOption { return func(r *RunRequest) { r.Parent = id } }

// InFilesRoot keeps the tools of the run in the root of the files tool,
// also with tools.files.perRun (RunRequest.FilesRoot).
func InFilesRoot() RunOption { return func(r *RunRequest) { r.FilesRoot = true } }

// MetaTitle is the metadata key that sets the title of a run. A surface
// that takes a title from a person puts it here.
const MetaTitle = "fw.title"

// MetaBatch is the metadata key of the job that a run belongs to. The
// front of a fleet gives every run of one job the same value when it puts
// the job on the queue, and a child run inherits the value of its parent,
// in this process or in another one, so one name covers the whole job.
const MetaBatch = "fw.batch"

// MaxTitleRunes is the length of a title that TitleFrom makes.
const MaxTitleRunes = 64

// TitleFrom makes a short title from the input of a run: the first
// sentence of the first line that has text, without Markdown marks (a
// heading, a list or quote mark, backticks, and bold), cut at
// a word boundary to MaxTitleRunes. It returns "" for an input without
// text.
func TitleFrom(input string) string {
	var line string
	for _, l := range strings.Split(input, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "#>*-+ \t"))
		if l != "" {
			line = l
			break
		}
	}
	if line == "" {
		return ""
	}
	// The first sentence ends at a stop that a space follows.
	for i := 0; i+1 < len(line); i++ {
		if (line[i] == '.' || line[i] == '?' || line[i] == '!') && line[i+1] == ' ' && i >= 12 {
			line = line[:i+1]
			break
		}
	}
	// Inline Markdown marks are noise in a title.
	line = strings.NewReplacer("`", "", "**", "", "__", "").Replace(line)
	line = strings.Join(strings.Fields(line), " ")
	line = strings.TrimSuffix(line, ".")
	r := []rune(line)
	if len(r) <= MaxTitleRunes {
		return line
	}
	cut := string(r[:MaxTitleRunes])
	if i := strings.LastIndexByte(cut, ' '); i >= MaxTitleRunes/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

// Limits of the metadata of a run.
const (
	MaxMetadataEntries    = 64
	MaxMetadataKeyBytes   = 128
	MaxMetadataValueBytes = 4096
)

// kernelMetadata lists the metadata keys that only the kernel sets: the
// idempotency key and the keys of a change of the text of a run
// (MetaDriftSystem, MetaDriftInstructions). A request and an annotation
// cannot set them. They do not count against MaxMetadataEntries, and
// neither do the keys that the kernel sets on a branch, MetaForkOf and
// MetaForkSeq, nor the keys of the bus intake (intake.go), which a retry
// carries in its request.
var kernelMetadata = []string{"idempotencyKey", MetaDriftSystem, MetaDriftInstructions}

// kernelMeta reports whether only the kernel sets the metadata key k.
func kernelMeta(k string) bool { return slices.Contains(kernelMetadata, k) }

// boundless reports whether the metadata key k does not count against
// MaxMetadataEntries, because the kernel sets it.
func boundless(k string) bool {
	switch k {
	case MetaForkOf, MetaForkSeq, MetaIntakeQueue, MetaIntakeAttempt, MetaIntakeRetryOf, MetaIntakeOutcome:
		return true
	}
	return kernelMeta(k)
}

// CheckMetadata bounds the metadata of a request with ErrInvalidRequest.
// The keys that the kernel sets are reserved: idempotencyKey holds the
// idempotency key, and MetaDriftSystem and MetaDriftInstructions record a
// change of the text of a run.
func CheckMetadata(m map[string]string) error {
	for k := range m {
		switch {
		case k == "idempotencyKey":
			return fmt.Errorf("%w: the metadata key idempotencyKey is reserved; use the idempotency key of the surface", ErrInvalidRequest)
		case kernelMeta(k):
			return fmt.Errorf("%w: the metadata key %s is reserved for the kernel", ErrInvalidRequest, k)
		}
	}
	return checkMetadataBounds(m)
}

// checkMetadataBounds checks the bounds of metadata with ErrInvalidRequest.
// A key that the kernel sets does not count against MaxMetadataEntries, so
// the metadata of a run that the kernel annotated or branched takes as many
// entries as before.
func checkMetadataBounds(m map[string]string) error {
	n := 0
	for k := range m {
		if !boundless(k) {
			n++
		}
	}
	if n > MaxMetadataEntries {
		return fmt.Errorf("%w: metadata has %d entries; the limit is %d", ErrInvalidRequest, n, MaxMetadataEntries)
	}
	for k, v := range m {
		switch {
		case k == "":
			return fmt.Errorf("%w: a metadata key must not be empty", ErrInvalidRequest)
		case len(k) > MaxMetadataKeyBytes:
			return fmt.Errorf("%w: a metadata key is longer than %d bytes", ErrInvalidRequest, MaxMetadataKeyBytes)
		case len(v) > MaxMetadataValueBytes:
			return fmt.Errorf("%w: the metadata value of %q is longer than %d bytes", ErrInvalidRequest, k, MaxMetadataValueBytes)
		}
	}
	return nil
}

// WithMetadata adds metadata.
func WithMetadata(m map[string]string) RunOption {
	return func(r *RunRequest) {
		if r.Metadata == nil {
			r.Metadata = map[string]string{}
		}
		for k, v := range m {
			r.Metadata[k] = v
		}
	}
}

// WithIdempotencyKey makes a retried start return the same run.
func WithIdempotencyKey(k string) RunOption { return func(r *RunRequest) { r.IdempotencyKey = k } }

// WithAttachments attaches files to the first message of the run. Each
// block comes from Attachment.
func WithAttachments(files ...Block) RunOption {
	return func(r *RunRequest) { r.Files = append(r.Files, files...) }
}

// ApprovalDecision answers a pending approval.
type ApprovalDecision struct {
	ApprovalID string          `json:"approvalId"`
	Decision   string          `json:"decision"` // "allow" or "deny"
	Note       string          `json:"note,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"` // form answers when the approval carried a schema
	Decider    string          `json:"decider,omitempty"`
}

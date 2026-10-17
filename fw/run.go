package fw

import (
	"encoding/json"
	"fmt"
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
type Budget struct {
	MaxTurns        int           `yaml:"maxTurns,omitempty" json:"maxTurns,omitempty"`
	MaxTokens       int           `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`
	MaxCostUSD      float64       `yaml:"maxCostUSD,omitempty" json:"maxCostUSD,omitempty"`
	Deadline        time.Duration `yaml:"deadline,omitempty" json:"deadline,omitempty"`
	MaxToolCalls    int           `yaml:"maxToolCalls,omitempty" json:"maxToolCalls,omitempty"`
	MaxCallsPerTurn int           `yaml:"maxCallsPerTurn,omitempty" json:"maxCallsPerTurn,omitempty"`
	MaxRepeats      int           `yaml:"maxRepeats,omitempty" json:"maxRepeats,omitempty"`
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
	return b
}

// Cap returns b with every limit that is larger than the same limit of o
// lowered to it. A limit that o leaves at zero stays as it is. The kernel
// caps the budget of a child run by the budget of its parent.
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
	return b
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
// child, else time.zone of the configuration.
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
}

// Overlay returns p with the set fields of o applied on top. A set System,
// Model, Effort, TimeZone, or Description replaces the value of p.
// Instructions are appended. A non-nil Tools or Skills list replaces the
// list of p. The policy is the stricter of the two, so an overlay can
// tighten the rules but cannot loosen them. The budget takes every set field of o, and the
// kernel caps the budget of a child run by its parent afterwards.
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

// RunUsage is the usage of a run so far.
type RunUsage struct {
	Usage
	Turns     int     `json:"turns"`
	ToolCalls int     `json:"toolCalls,omitempty"`
	CostUSD   float64 `json:"costUSD,omitempty"`
}

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
}

// RunRequest describes a run to start.
//
// Instructions is text for this run alone. It follows the system prompt of
// the profile. Profile is an inline profile that overlays the named profile
// of Agent, so a caller shapes an agent for one run without a configuration
// change: a system prompt, a tool subset, a model, a policy, or a budget.
// The kernel keeps both in fw.run.queued, so a resume and a replay see them.
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
	Parent         string            `json:"parent,omitempty"`
	IdempotencyKey string            `json:"idempotencyKey,omitempty"`
	Files          []Block           `json:"files,omitempty"` // attachments of the first message; see Attachment
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

// MetaTitle is the metadata key that sets the title of a run. A surface
// that takes a title from a person puts it here.
const MetaTitle = "fw.title"

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

// CheckMetadata bounds the metadata of a request with ErrInvalidRequest.
// The key idempotencyKey is the kernel's: it holds the idempotency key.
func CheckMetadata(m map[string]string) error {
	if len(m) > MaxMetadataEntries {
		return fmt.Errorf("%w: metadata has %d entries; the limit is %d", ErrInvalidRequest, len(m), MaxMetadataEntries)
	}
	for k, v := range m {
		switch {
		case k == "":
			return fmt.Errorf("%w: a metadata key must not be empty", ErrInvalidRequest)
		case k == "idempotencyKey":
			return fmt.Errorf("%w: the metadata key idempotencyKey is reserved; use the idempotency key of the surface", ErrInvalidRequest)
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

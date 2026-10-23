// Package native serves the native HTTP API of fw under /v1. See
// docs/ARCHITECTURE.md section 8.2.
//
// Every route requires the agent's authentication. Every response is JSON,
// except the event streams, which are server-sent events. Every error is an
// RFC 9457 problem document.
package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/bdubs00/featherweight-kernel/memory"
	"github.com/bdubs00/featherweight-kernel/surface/view"
)

// Name is the registered surface name.
const Name = "native"

// Limits of a request.
const (
	MaxBodyBytes         = 1 << 20                                  // 1 MB
	MaxFilesBodyBytes    = fw.MaxAttachmentBytes/3*4 + MaxBodyBytes // a run or a message with files, in base64
	MaxIdempotencyKeyLen = 255
	maxInstructionBytes  = 256 << 10 // the kernel limit of a system prompt or instructions
	maxGlobs             = 64        // the kernel limit of tool and skill globs
	maxGlobBytes         = 256
	maxRules             = 256
	maxRuleBytes         = 1024
)

// KeepAlive is the interval of the keep-alive comment on an event stream.
const KeepAlive = 15 * time.Second

// HeaderIdempotencyKey makes POST /v1/runs safe to retry.
const HeaderIdempotencyKey = "Idempotency-Key"

// HeaderOnBehalfOf names the person for whom a trusted caller acts, by the
// subject of the person (fw.WithActingFor). The front of a fleet decides
// the action of a person, then relays it with the static token and this
// header, so the ledger records the person: the decider of an approval,
// the by of a follow-up, of a change of the budget, of a cancel, and of a
// reaction, and the person of a branch. The kernel still authorizes the
// caller alone. A person who names another subject gets a 403, and an
// empty header or the subject of the caller changes nothing. A start names
// its person in the body, not here.
const HeaderOnBehalfOf = "Fw-On-Behalf-Of"

func init() {
	fw.RegisterSurface(Name, func(*fw.Config) (fw.Surface, error) { return New(), nil })
}

// New returns the native API surface with every registered extension.
func New() fw.Surface { return newSurface(KeepAlive, registered()) }

// NewWith returns the native API surface with the extensions exts and
// without the registered ones. A test passes its own extension with it.
func NewWith(exts ...Extension) fw.Surface { return newSurface(KeepAlive, exts) }

// newSurface builds the surface. The options change the server before it
// mounts, so a test can set the keep-alive interval.
func newSurface(keepAlive time.Duration, exts []Extension, opts ...func(*server)) fw.Surface {
	return fw.SurfaceFunc(func(mux *http.ServeMux, a *fw.Agent) error {
		s := &server{a: a, keepAlive: keepAlive}
		s.mem, s.memErr = memory.New(a.Config())
		for _, o := range opts {
			o(s)
		}
		return s.mount(mux, exts)
	})
}

type server struct {
	a          *fw.Agent
	keepAlive  time.Duration
	signinPath string // set by an extension that signs a person in
	relay      Relay  // set by an extension, see Relay

	mem    *memory.Store // the memory of the agents (memory.go), nil when memErr is set
	memErr error
}

// remote wraps the handler of a route about one run. When the relay knows
// the run as one of another process, the relay answers the request.
func (s *server) remote(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.relay != nil && s.relay.Take(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *server) mount(mux *http.ServeMux, exts []Extension) error {
	routes := []struct {
		pattern string
		h       http.HandlerFunc
	}{
		{"POST /v1/runs", s.createRun},
		{"GET /v1/runs", s.listRuns},
		{"GET /v1/runs/{id}", s.remote(s.getRun)},
		{"GET /v1/runs/{id}/events", s.remote(s.runEvents)},
		{"GET /v1/runs/{id}/live", s.remote(s.live)},
		{"POST /v1/runs/{id}/messages", s.remote(s.sendMessage)},
		{"POST /v1/runs/{id}/reactions", s.remote(s.react)},
		{"PATCH /v1/runs/{id}/budget", s.remote(s.changeBudget)},
		{"POST /v1/runs/{id}/approvals/{approval}", s.remote(s.decide)},
		{"POST /v1/runs/{id}/cancel", s.remote(s.cancel)},
		{"POST /v1/runs/{id}/fork", s.remote(s.fork)},
		{"POST /v1/runs/fork", s.forkEvents},
		{"GET /v1/runs/{id}/files", s.remote(s.runFiles)},
		{"GET /v1/runs/{id}/files/{path...}", s.remote(s.runFile)},
		{"GET /v1/agents", s.listAgents},
		{"GET /v1/agents/{name}", s.getAgent},
		{"GET /v1/status", s.status},
		{"GET /v1/tools", s.listTools},
		{"GET /v1/skills", s.listSkills},
		{"GET /v1/skills/{name...}", s.getSkill},
	}
	routes = append(routes, s.memoryRoutes()...)
	for _, rt := range routes {
		mux.Handle(rt.pattern, s.protect(rt.h))
	}
	for _, e := range exts {
		if err := e(&Kit{s: s, mux: mux}); err != nil {
			return fmt.Errorf("native: mount an extension: %w", err)
		}
	}
	return nil
}

// protect puts a handler behind the agent's Authn. When the Authn writes a
// 401 with no body, the response gets a problem document. When an
// extension signs a person in, the document names its route in the field
// signin. After the authentication, the header HeaderOnBehalfOf names the
// person for whom the caller acts (onBehalfOf).
func (s *server) protect(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &challengeWriter{ResponseWriter: w}
		fw.RequireAuth(s.a, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r, ok := s.onBehalfOf(w, r); ok {
				next(w, r)
			}
		})).ServeHTTP(cw, r)
		if cw.problem && !cw.wrote {
			p := newProblem(r, http.StatusUnauthorized, "the request carries no valid credential")
			if s.signinPath != "" {
				p.Signin = s.signinPath
			}
			_ = json.NewEncoder(w).Encode(p)
		}
	})
}

// onBehalfOf puts the person of the header HeaderOnBehalfOf in the context
// of the request (fw.WithActingFor) and returns the request. A request
// without the header passes as it is. On failure it writes the problem and
// returns false.
func (s *server) onBehalfOf(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	values := r.Header.Values(HeaderOnBehalfOf)
	if len(values) == 0 {
		return r, true
	}
	if len(values) > 1 {
		problem(w, r, http.StatusBadRequest, "the "+HeaderOnBehalfOf+" header names one person, and the request has it more than once")
		return nil, false
	}
	ctx, err := fw.WithActingFor(r.Context(), values[0])
	switch {
	case errors.Is(err, fw.ErrInvalidRequest):
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the %s header holds more than %d bytes or a control character, or is not UTF-8", HeaderOnBehalfOf, fw.MaxActingForBytes))
		return nil, false
	case err != nil:
		s.fail(w, r, err)
		return nil, false
	}
	return r.WithContext(ctx), true
}

// challengeWriter sees the response of a failed authentication.
type challengeWriter struct {
	http.ResponseWriter
	problem bool // the status is 401 and the body is a problem document
	wrote   bool // the Authn wrote a body
}

func (c *challengeWriter) WriteHeader(code int) {
	if code == http.StatusUnauthorized && c.Header().Get("Content-Type") == "" {
		c.Header().Set("Content-Type", problemContentType)
		c.problem = true
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *challengeWriter) Write(p []byte) (int, error) {
	c.wrote = true
	return c.ResponseWriter.Write(p)
}

// MaxTitleRunes bounds the field title of POST /v1/runs.
const MaxTitleRunes = 200

// createBody is the body of POST /v1/runs.
//
// Instructions is text for this run alone; it follows the system prompt.
// Profile is an inline profile that overlays the named profile of agent for
// this run. Budget limits the run; a set field wins over the same limit of
// the profile. Title names the work; it goes into the metadata key
// fw.MetaTitle and wins over a title in metadata. Outcome asks for a grade
// of the answer by a grader run, and replaces the outcome of the profile
// (fw.Outcome); the kernel checks it.
type createBody struct {
	Agent        string            `json:"agent"`
	Input        string            `json:"input"`
	Title        string            `json:"title"`
	Instructions string            `json:"instructions"`
	Profile      *profileBody      `json:"profile"`
	Budget       *budgetBody       `json:"budget"`
	Metadata     map[string]string `json:"metadata"`
	Wait         bool              `json:"wait"`
	Files        []fileBody        `json:"files"`
	Outcome      *fw.Outcome       `json:"outcome"`
	// Principal and PrincipalKind name the person that the run is for. Only
	// a trusted caller, the static token or a workload, sets them; a parent
	// in another process does, so the child acts as its person.
	Principal     string `json:"principal"`
	PrincipalKind string `json:"principalKind"`
	// ID is the id of the run. Only a trusted caller sets it; a parent in
	// another process does, because it records the id of its child before
	// the child has the run.
	ID string `json:"id"`
	// Check makes the run a check run (fw.Check), whose input can be
	// empty.
	Check *checkBody `json:"check"`
}

// checkBody is the check of a request: fw.Check with the timeout as a
// duration string, such as "30s", or a number of nanoseconds. The kernel
// checks the bounds.
type checkBody struct {
	Steps    []fw.CheckStep `json:"steps"`
	Timeout  checkTimeoutIn `json:"timeout"`
	MaxBytes int            `json:"maxBytes"`
}

// checkTimeoutIn is the timeout of a check body.
type checkTimeoutIn time.Duration

func (d *checkTimeoutIn) UnmarshalJSON(b []byte) error {
	v, err := parseDuration(b, "check.timeout")
	*d = checkTimeoutIn(v)
	return err
}

// check converts the body.
func (c *checkBody) check() *fw.Check {
	if c == nil {
		return nil
	}
	return &fw.Check{Steps: c.Steps, Timeout: time.Duration(c.Timeout), MaxBytes: c.MaxBytes}
}

// fileBody is one attachment of a run or a message (section 7.10). Data is
// base64 in JSON. The kernel sniffs the media type from the bytes.
type fileBody struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// attachments converts the files of a body to file blocks.
func attachments(files []fileBody) []fw.Block {
	out := make([]fw.Block, 0, len(files))
	for _, f := range files {
		out = append(out, fw.Attachment(f.Name, f.Data))
	}
	return out
}

// profileBody is the inline profile of a request: fw.AgentProfile without
// a name or a description, with a budget in the request shape.
type profileBody struct {
	System       string      `json:"system"`
	Instructions []string    `json:"instructions"`
	Skills       []string    `json:"skills"`
	Tools        []string    `json:"tools"`
	Model        string      `json:"model"`
	Effort       string      `json:"effort"`
	TimeZone     string      `json:"timeZone"`
	Policy       *rulesBody  `json:"policy"`
	Budget       *budgetBody `json:"budget"`
	// Compaction overlays the compaction block of the named profile
	// (fw.CompactionConfig); the kernel checks it.
	Compaction *fw.CompactionConfig `json:"compaction"`
}

// rulesBody is fw.Rules: a default verdict and three rule lists, plus All,
// the list of independent clauses that must each agree, nested.
type rulesBody struct {
	Default string      `json:"default"`
	Allow   []string    `json:"allow"`
	Ask     []string    `json:"ask"`
	Deny    []string    `json:"deny"`
	All     []rulesBody `json:"all"`
}

// budgetBody is fw.Budget with a deadline and a report timeout that are Go
// duration strings, such as "90s", or numbers of nanoseconds as in
// fw.Budget. FinalReport, ReportMaxTokens, and ReportTimeout shape the
// final report call at a limit of turns, tool calls, or working time.
type budgetBody struct {
	MaxTurns        int             `json:"maxTurns"`
	MaxTokens       int             `json:"maxTokens"`
	MaxCostUSD      float64         `json:"maxCostUSD"`
	Deadline        duration        `json:"deadline"`
	MaxToolCalls    int             `json:"maxToolCalls"`
	MaxCallsPerTurn int             `json:"maxCallsPerTurn"`
	MaxRepeats      int             `json:"maxRepeats"`
	FinalReport     *bool           `json:"finalReport"`
	ReportMaxTokens int             `json:"reportMaxTokens"`
	ReportTimeout   reportTimeoutIn `json:"reportTimeout"`
}

// duration is the deadline of a budget body.
type duration time.Duration

func (d *duration) UnmarshalJSON(b []byte) error {
	v, err := parseDuration(b, "deadline")
	*d = duration(v)
	return err
}

// reportTimeoutIn is the report timeout of a budget body.
type reportTimeoutIn time.Duration

func (d *reportTimeoutIn) UnmarshalJSON(b []byte) error {
	v, err := parseDuration(b, "reportTimeout")
	*d = reportTimeoutIn(v)
	return err
}

// parseDuration reads a duration string or a number of nanoseconds. The
// field names the value in an error.
func parseDuration(b []byte, field string) (time.Duration, error) {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("%s %q is not a duration such as \"90s\" or \"2h\"", field, s)
		}
		return v, nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return 0, fmt.Errorf("%s is a duration string such as \"90s\" or a number of nanoseconds", field)
	}
	return time.Duration(n), nil
}

// budget converts the body. The field names the budget in a problem, so
// a profile budget reads "profile.budget".
func (b *budgetBody) budget(field string) (*fw.Budget, error) {
	if b == nil {
		return nil, nil
	}
	if b.MaxTurns < 0 || b.MaxTokens < 0 || b.MaxCostUSD < 0 || b.Deadline < 0 || b.MaxToolCalls < 0 || b.MaxCallsPerTurn < 0 || b.MaxRepeats < 0 ||
		b.ReportMaxTokens < 0 || b.ReportTimeout < 0 {
		return nil, fmt.Errorf("%s limits must not be negative", field)
	}
	return &fw.Budget{
		MaxTurns:        b.MaxTurns,
		MaxTokens:       b.MaxTokens,
		MaxCostUSD:      b.MaxCostUSD,
		Deadline:        time.Duration(b.Deadline),
		MaxToolCalls:    b.MaxToolCalls,
		MaxCallsPerTurn: b.MaxCallsPerTurn,
		MaxRepeats:      b.MaxRepeats,
		FinalReport:     b.FinalReport,
		ReportMaxTokens: b.ReportMaxTokens,
		ReportTimeout:   time.Duration(b.ReportTimeout),
	}, nil
}

// profile validates the body and converts it. The kernel validates the
// result again, so a limit here is the same limit with a clearer message.
func (p *profileBody) profile() (*fw.AgentProfile, error) {
	if p == nil {
		return nil, nil
	}
	if len(p.System) > maxInstructionBytes {
		return nil, fmt.Errorf("profile.system is longer than %d bytes", maxInstructionBytes)
	}
	total := 0
	for i, s := range p.Instructions {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("profile.instructions[%d] is empty", i)
		}
		total += len(s)
	}
	if total > maxInstructionBytes {
		return nil, fmt.Errorf("profile.instructions are longer than %d bytes in total", maxInstructionBytes)
	}
	if err := checkGlobs("profile.tools", p.Tools); err != nil {
		return nil, err
	}
	if err := checkGlobs("profile.skills", p.Skills); err != nil {
		return nil, err
	}
	if !fw.ValidEffort(p.Effort) {
		return nil, fmt.Errorf("profile.effort %q is not one of %s", p.Effort, strings.Join(fw.Efforts, ", "))
	}
	if strings.ContainsAny(p.Model, " \t\r\n") {
		return nil, fmt.Errorf("profile.model %q is not a provider/model selector or a name from models", p.Model)
	}
	zone := strings.TrimSpace(p.TimeZone)
	if zone != "" {
		if _, _, err := fw.LoadZone(zone); err != nil {
			return nil, fmt.Errorf("profile.timeZone %q is not an IANA time zone such as America/Chicago, or UTC", p.TimeZone)
		}
	}
	out := &fw.AgentProfile{
		System:       p.System,
		Instructions: p.Instructions,
		Skills:       p.Skills,
		Tools:        p.Tools,
		Model:        p.Model,
		Effort:       strings.ToLower(p.Effort),
		TimeZone:     zone,
	}
	if p.Policy != nil {
		rules, err := p.Policy.rules()
		if err != nil {
			return nil, err
		}
		out.Policy = rules
	}
	b, err := p.Budget.budget("profile.budget")
	if err != nil {
		return nil, err
	}
	out.Budget = b
	if p.Compaction != nil {
		c := *p.Compaction
		c.Model = strings.TrimSpace(c.Model)
		if err := fw.CheckCompaction(nil, &c); err != nil {
			return nil, errors.New("profile." + strings.TrimPrefix(err.Error(), fw.ErrInvalidRequest.Error()+": "))
		}
		out.Compaction = &c
	}
	return out, nil
}

// checkGlobs bounds a list of tool or skill globs.
func checkGlobs(field string, globs []string) error {
	if len(globs) > maxGlobs {
		return fmt.Errorf("%s lists %d globs; the limit is %d", field, len(globs), maxGlobs)
	}
	for i, g := range globs {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("%s[%d] is empty", field, i)
		}
		if len(g) > maxGlobBytes {
			return fmt.Errorf("%s[%d] is longer than %d bytes", field, i, maxGlobBytes)
		}
	}
	return nil
}

// maxRulesDepth bounds how deeply All clauses nest in a request profile.
const maxRulesDepth = 8

// rules validates the body and converts it.
func (r *rulesBody) rules() (*fw.Rules, error) {
	return r.rulesAt(0)
}

// rulesAt converts the body at a nesting depth of All clauses.
func (r *rulesBody) rulesAt(depth int) (*fw.Rules, error) {
	if depth > maxRulesDepth {
		return nil, fmt.Errorf("profile.policy.all nests more than %d levels", maxRulesDepth)
	}
	def := strings.ToLower(strings.TrimSpace(r.Default))
	switch def {
	case "", "allow", "ask", "deny":
	default:
		return nil, fmt.Errorf("profile.policy.default %q is not allow, ask, or deny", r.Default)
	}
	for _, l := range []struct {
		name  string
		rules []string
	}{{"allow", r.Allow}, {"ask", r.Ask}, {"deny", r.Deny}} {
		if len(l.rules) > maxRules {
			return nil, fmt.Errorf("profile.policy.%s lists %d rules; the limit is %d", l.name, len(l.rules), maxRules)
		}
		for i, rule := range l.rules {
			if strings.TrimSpace(rule) == "" {
				return nil, fmt.Errorf("profile.policy.%s[%d] is empty", l.name, i)
			}
			if len(rule) > maxRuleBytes {
				return nil, fmt.Errorf("profile.policy.%s[%d] is longer than %d bytes", l.name, i, maxRuleBytes)
			}
		}
	}
	out := &fw.Rules{Default: def, Allow: r.Allow, Ask: r.Ask, Deny: r.Deny}
	for i := range r.All {
		c, err := r.All[i].rulesAt(depth + 1)
		if err != nil {
			return nil, fmt.Errorf("profile.policy.all[%d]: %w", i, err)
		}
		out.All = append(out.All, *c)
	}
	return out, nil
}

func (s *server) createRun(w http.ResponseWriter, r *http.Request) {
	if s.relay != nil && s.relay.Take(w, r) {
		return // a start on another process, see Relay
	}
	var body createBody
	if !s.decodeFiles(w, r, &body, func() int { return len(body.Files) }) {
		return
	}
	if strings.TrimSpace(body.Input) == "" && len(body.Files) == 0 && body.Check == nil {
		problem(w, r, http.StatusBadRequest, "the field input is required and must not be empty")
		return
	}
	if body.ID != "" {
		if !trustedCaller(r) {
			problem(w, r, http.StatusForbidden, "only the static token or a workload can name the id of a run")
			return
		}
		if _, ok := fw.IDTime(body.ID); !ok {
			problem(w, r, http.StatusBadRequest, "the field id is not an id of a run")
			return
		}
	}
	if len(body.Instructions) > maxInstructionBytes {
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the field instructions is longer than %d bytes", maxInstructionBytes))
		return
	}
	profile, err := body.Profile.profile()
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	budget, err := body.Budget.budget("budget")
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	title := strings.TrimSpace(body.Title)
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		problem(w, r, http.StatusBadRequest, fmt.Sprintf("the field title is longer than %d characters", MaxTitleRunes))
		return
	}
	if title != "" {
		meta := make(map[string]string, len(body.Metadata)+1)
		maps.Copy(meta, body.Metadata)
		meta[fw.MetaTitle] = title
		body.Metadata = meta
	}
	if err := fw.CheckMetadata(body.Metadata); err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	req := fw.RunRequest{
		ID:            body.ID,
		Agent:         body.Agent,
		Input:         body.Input,
		Instructions:  body.Instructions,
		Profile:       profile,
		Budget:        budget,
		Metadata:      body.Metadata,
		Files:         attachments(body.Files),
		Outcome:       body.Outcome,
		Principal:     strings.TrimSpace(body.Principal),
		PrincipalKind: strings.TrimSpace(body.PrincipalKind),
		Check:         body.Check.check(),
	}
	run, ok := s.start(w, r, req)
	if !ok {
		return
	}
	switch {
	case acceptsEventStream(r):
		s.stream(w, r, run.ID, 0)
	case body.Wait:
		run, err = s.a.Wait(r.Context(), run.ID)
		if err != nil {
			if r.Context().Err() != nil {
				return // the client is gone
			}
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, run)
	default:
		w.Header().Set("Location", "/v1/runs/"+run.ID)
		writeJSON(w, http.StatusCreated, run)
	}
}

// start starts a run with the fields that every start through this surface
// shares: the origin, the Idempotency-Key header, and the principal. On
// failure it writes the problem and returns false.
func (s *server) start(w http.ResponseWriter, r *http.Request, req fw.RunRequest) (*fw.Run, bool) {
	key := r.Header.Get(HeaderIdempotencyKey)
	if err := checkIdempotencyKey(key); err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return nil, false
	}
	req.Origin = fw.OriginHTTP
	req.IdempotencyKey = key
	if p := fw.PrincipalFrom(r.Context()); p != nil {
		// A trusted process, the static token or a workload, starts a run for
		// a person when it names one: a parent in another Task does. Anyone
		// else starts the run as the principal of the request.
		trusted := p.Kind == fw.KindToken || p.Kind == fw.KindWorkload
		if !trusted || strings.TrimSpace(req.Principal) == "" {
			req.Principal, req.PrincipalKind = p.Subject, ""
		}
	}
	run, err := s.a.Start(r.Context(), req.Input, fw.WithRequest(req))
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return run, true
}

func checkIdempotencyKey(k string) error {
	if len(k) > MaxIdempotencyKeyLen {
		return fmt.Errorf("the Idempotency-Key header is longer than %d bytes", MaxIdempotencyKeyLen)
	}
	for _, c := range k {
		if c > unicode.MaxASCII || !unicode.IsPrint(c) {
			return errors.New("the Idempotency-Key header must hold printable ASCII characters only")
		}
	}
	return nil
}

// acceptsEventStream reports whether the Accept header names
// text/event-stream.
func acceptsEventStream(r *http.Request) bool { return accepts(r, "text/event-stream") }

// accepts reports whether the Accept header names the media type.
func accepts(r *http.Request, mediaType string) bool {
	for _, v := range r.Header.Values("Accept") {
		for part := range strings.SplitSeq(v, ",") {
			mt, _, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err == nil && mt == mediaType {
				return true
			}
		}
	}
	return false
}

// listRuns lists the runs that the caller can view, newest first, or with
// the query parameter parent the child runs of one run. The parameters
// agent, state, principal, batch, and kind narrow the list; the state
// active matches every run that is not finished. Without kind, the list
// leaves out the check runs, and kind=check lists them alone. The
// parameter limit bounds the list.
// else (others), as section 13.6 says. The answer also holds total and
// glance, "At a glance", over every run that matches, before the limit
// cuts the list.
func (s *server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var runs []*fw.Run
	var err error
	if parent := q.Get("parent"); parent != "" {
		runs, err = s.a.Children(r.Context(), parent)
		if errors.Is(err, fw.ErrRunNotFound) {
			problem(w, r, http.StatusNotFound, "no run has the id "+strconv.Quote(parent))
			return
		}
	} else {
		runs, err = s.a.List(r.Context())
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			problem(w, r, http.StatusBadRequest, "the query parameter limit must be a positive number")
			return
		}
		limit = n
	}
	matched := view.FilterKind(view.FilterBatch(view.Filter(runs, q.Get("agent"), q.Get("state"), q.Get("principal"), 0), q.Get("batch")), q.Get("kind"))
	glance, total := view.GlanceOf(matched, time.Now()), len(matched)
	if limit > 0 && len(matched) > limit {
		matched = matched[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": matched, "total": total, "glance": glance})
}

func (s *server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.a.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// runEvents streams the ledger of a run. The header Last-Event-ID resumes
// after that sequence number. The query parameter from starts at that
// sequence number. Last-Event-ID wins, because a reconnecting EventSource
// sends it with the original URL.
//
// A client that accepts application/json and not text/event-stream gets a
// page of the ledger instead of a stream: the run object and the events
// from the sequence number from, at most limit of them (see eventsPage).
func (s *server) runEvents(w http.ResponseWriter, r *http.Request) {
	var from uint64
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			problem(w, r, http.StatusBadRequest, "the query parameter from must be a sequence number")
			return
		}
		from = n
	}
	if v := strings.TrimSpace(r.Header.Get("Last-Event-ID")); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == ^uint64(0) {
			problem(w, r, http.StatusBadRequest, "the Last-Event-ID header must be a sequence number")
			return
		}
		from = n + 1
	}
	if accepts(r, "application/json") && !acceptsEventStream(r) {
		s.eventsPage(w, r, r.PathValue("id"), from)
		return
	}
	s.stream(w, r, r.PathValue("id"), from)
}

// messageBody is the body of POST /v1/runs/{id}/messages. Content is a
// string or an array of content blocks. Content can be empty when the
// message has files. Budget changes the budget of the run with the message
// (fw.WithSendBudget): each field that it sets replaces the same limit for
// the work that follows. From names the party that wrote the message when
// the caller relays it (fw.WithMessageFrom). Only a trusted caller, the
// static token or a workload, sets it, as for the principal and the id of
// a run. Pinned keeps the message through every compaction
// (fw.WithMessagePinned); any caller who can send sets it.
type messageBody struct {
	Content json.RawMessage `json:"content"`
	Files   []fileBody      `json:"files"`
	Budget  *budgetBody     `json:"budget"`
	From    *fw.MessageFrom `json:"from"`
	Pinned  bool            `json:"pinned"`
}

// trustedCaller reports whether the caller of the request is the static
// token or a workload, which can name another party in a body: the person
// of a run, the id of a run, the sender of a message, or the person of a
// reaction.
func trustedCaller(r *http.Request) bool {
	p := fw.PrincipalFrom(r.Context())
	return p != nil && (p.Kind == fw.KindToken || p.Kind == fw.KindWorkload)
}

func (s *server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body messageBody
	if !s.decodeFiles(w, r, &body, func() int { return len(body.Files) }) {
		return
	}
	var blocks []fw.Block
	if raw := bytes.TrimSpace(body.Content); len(body.Files) == 0 || (len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`))) {
		var err error
		if blocks, err = contentBlocks(body.Content); err != nil {
			problem(w, r, http.StatusBadRequest, err.Error())
			return
		}
	}
	blocks = append(blocks, attachments(body.Files)...)
	budget, err := body.Budget.budget("budget")
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	var opts []fw.SendOption
	if budget != nil {
		opts = append(opts, fw.WithSendBudget(*budget))
	}
	if body.From != nil {
		if !trustedCaller(r) {
			problem(w, r, http.StatusForbidden, "only the static token or a workload can name the sender of a message")
			return
		}
		opts = append(opts, fw.WithMessageFrom(*body.From))
	}
	if body.Pinned {
		opts = append(opts, fw.WithMessagePinned())
	}
	id := r.PathValue("id")
	if err := s.a.Send(r.Context(), id, fw.Message{Role: fw.RoleUser, Content: blocks}, fw.OriginHTTP, opts...); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeRun(w, r, http.StatusAccepted, id)
}

// reactionBody is the body of POST /v1/runs/{id}/reactions: the event of
// the run, 0 for the run as a whole, the reaction, up, down, or heart, a
// note, and the person. Only a trusted caller, the static token or a
// workload, names the person in by, as for the sender of a message;
// anyone else reacts as the principal of the request. Without by, the
// reaction names the person of HeaderOnBehalfOf, or the caller.
type reactionBody struct {
	Seq      uint64 `json:"seq"`
	Reaction string `json:"reaction"`
	Note     string `json:"note"`
	By       string `json:"by"`
}

// react answers POST /v1/runs/{id}/reactions with the run object after the
// reaction (fw.Agent.React). The caller needs the operator role on the
// run.
func (s *server) react(w http.ResponseWriter, r *http.Request) {
	var body reactionBody
	if !s.decode(w, r, &body) {
		return
	}
	by := strings.TrimSpace(body.By)
	if by != "" && !trustedCaller(r) {
		problem(w, r, http.StatusForbidden, "only the static token or a workload can name the person of a reaction")
		return
	}
	id := r.PathValue("id")
	d := fw.ReactionData{Seq: body.Seq, Reaction: strings.ToLower(strings.TrimSpace(body.Reaction)), Note: body.Note, By: by}
	if err := s.a.React(r.Context(), id, d); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeRun(w, r, http.StatusOK, id)
}

// budgetAnswer is the answer of PATCH /v1/runs/{id}/budget: the effective
// budget of the run after the change and the run object.
type budgetAnswer struct {
	Budget fw.Budget `json:"budget"`
	Run    *fw.Run   `json:"run"`
}

// changeBudget answers PATCH /v1/runs/{id}/budget: the body is a budget,
// and each field that it sets replaces the same limit of the run for the
// work that follows (fw.Agent.ChangeBudget). The caller needs the operator
// role on the run, as for a decision.
func (s *server) changeBudget(w http.ResponseWriter, r *http.Request) {
	var body budgetBody
	if !s.decode(w, r, &body) {
		return
	}
	change, err := body.budget("the budget")
	if err != nil {
		problem(w, r, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	b, err := s.a.ChangeBudget(r.Context(), id, *change, fw.OriginHTTP)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	run, err := s.a.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, budgetAnswer{Budget: b, Run: run})
}

// contentBlocks reads a string or an array of blocks.
func contentBlocks(raw json.RawMessage) ([]fw.Block, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, errors.New("the field content is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, errors.New("the field content is not a valid string")
		}
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("the field content must not be empty")
		}
		return []fw.Block{fw.Text(text)}, nil
	}
	var blocks []fw.Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, errors.New("the field content is a string or an array of content blocks")
	}
	if len(blocks) == 0 {
		return nil, errors.New("the field content must not be empty")
	}
	for i, b := range blocks {
		switch b.Type {
		case fw.BlockText, fw.BlockImage, fw.BlockDocument:
		default:
			return nil, fmt.Errorf("content block %d has type %q; a message holds text, image, and document blocks", i, b.Type)
		}
	}
	return blocks, nil
}

// decisionBody is the body of POST /v1/runs/{id}/approvals/{approval}.
type decisionBody struct {
	Decision string          `json:"decision"`
	Note     string          `json:"note"`
	Content  json.RawMessage `json:"content"`
}

func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	var body decisionBody
	if !s.decode(w, r, &body) {
		return
	}
	decision := strings.ToLower(strings.TrimSpace(body.Decision))
	content := bytes.TrimSpace(body.Content)
	if bytes.Equal(content, []byte("null")) {
		content = nil
	}
	switch {
	case decision == "allow" || decision == "deny":
	case decision == "" && len(content) > 0:
		// Form answers without a decision allow, as in the kernel.
		decision = "allow"
	default:
		problem(w, r, http.StatusBadRequest, `the field decision must be "allow" or "deny"`)
		return
	}
	// The kernel names the decider: the person of HeaderOnBehalfOf, or the
	// caller.
	d := fw.ApprovalDecision{
		ApprovalID: r.PathValue("approval"),
		Decision:   decision,
		Note:       body.Note,
		Content:    json.RawMessage(content),
	}
	id := r.PathValue("id")
	if err := s.a.Decide(r.Context(), id, d); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeRun(w, r, http.StatusOK, id)
}

// fork answers POST /v1/runs/{id}/fork with a branch of the run: a new run
// that keeps the events of the run up to seq and goes on with the message
// input, with the model, or the same way again (fw.Agent.Fork).
func (s *server) fork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq      uint64            `json:"seq"`
		Input    string            `json:"input"`
		Model    string            `json:"model"`
		Metadata map[string]string `json:"metadata"`
		Spawn    bool              `json:"spawn"`
	}
	if !s.decode(w, r, &body) {
		return
	}
	run, err := s.a.Fork(r.Context(), r.PathValue("id"), fw.ForkRequest{Seq: body.Seq, Input: body.Input, Model: body.Model, Metadata: body.Metadata, Spawn: body.Spawn, Origin: fw.OriginHTTP})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if body.Spawn {
		// The branch is placed, and its process takes it in the background.
		status = http.StatusAccepted
	}
	s.writeRun(w, r, status, run.ID)
}

// MaxForkBodyBytes bounds the body of POST /v1/runs/fork, which carries the
// kept events of a branch from another process.
const MaxForkBodyBytes = 64 << 20

// forkEvents answers POST /v1/runs/fork: a branch from the events that
// another process kept (fw.Agent.ForkEvents). Only a trusted caller, the
// static token or a workload, can send it, because the events name the
// person of the branch.
func (s *server) forkEvents(w http.ResponseWriter, r *http.Request) {
	p := fw.PrincipalFrom(r.Context())
	if p == nil || (p.Kind != fw.KindToken && p.Kind != fw.KindWorkload) {
		problem(w, r, http.StatusForbidden, "only the static token or a workload can start a branch from events")
		return
	}
	var body struct {
		ID       string            `json:"id"`
		Events   []fw.Event        `json:"events"`
		Input    string            `json:"input"`
		Model    string            `json:"model"`
		Metadata map[string]string `json:"metadata"`
	}
	if !s.decodeLimit(w, r, &body, MaxForkBodyBytes) {
		return
	}
	run, err := s.a.ForkEvents(r.Context(), body.Events, fw.ForkRequest{ID: body.ID, Input: body.Input, Model: body.Model, Metadata: body.Metadata, Origin: fw.OriginHTTP})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeRun(w, r, http.StatusCreated, run.ID)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.a.Cancel(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeRun(w, r, http.StatusOK, id)
}

// listAgents lists the profiles that the caller can view, each with the
// role of the caller and the usage of its visible runs.
func (s *server) listAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := view.Agents(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// listTools answers GET /v1/tools with the capability catalog: each tool
// spec of the runtime with the field text, which says in plain words what
// the tool does (view.Catalog).
func (s *server) listTools(w http.ResponseWriter, r *http.Request) {
	tools, err := view.Catalog(r.Context(), s.a)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tools == nil {
		tools = []view.Capability{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

// skillInfo is one entry of GET /v1/skills.
type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Origin is local for a skill of the skill directories and mcp for a
	// skill of an MCP server, which also names the server and the URI of
	// its SKILL.md.
	Origin string `json:"origin"`
	Server string `json:"server,omitempty"`
	URI    string `json:"uri,omitempty"`
}

// infoOf returns the skill as a listing names it.
func infoOf(sk fw.Skill) skillInfo {
	return skillInfo{Name: sk.Name, Description: sk.Description, Origin: sk.OriginName(), Server: sk.Server, URI: sk.URI}
}

// skillDoc is the JSON shape of GET /v1/skills/{name}.
type skillDoc struct {
	skillInfo
	Body  string   `json:"body"`
	Files []string `json:"files"`
}

// listSkills lists the skills of the configured skill directories.
func (s *server) listSkills(w http.ResponseWriter, _ *http.Request) {
	skills := s.a.Skills()
	out := make([]skillInfo, 0, len(skills))
	for _, sk := range skills {
		out = append(out, infoOf(sk))
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

// getSkill returns the SKILL.md file of a skill as text/markdown. When the
// client accepts application/json, it returns the body with the list of
// the other files of the skill.
func (s *server) getSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	sk, ok := s.a.SkillNamed(name)
	if !ok {
		problem(w, r, http.StatusNotFound, "no skill has the name "+strconv.Quote(name))
		return
	}
	body, err := sk.Body()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !accepts(r, "application/json") {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		return
	}
	files, err := sk.Files()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if files == nil {
		files = []string{}
	}
	writeJSON(w, http.StatusOK, skillDoc{skillInfo: infoOf(sk), Body: body, Files: files})
}

// writeRun writes the current status of a run.
func (s *server) writeRun(w http.ResponseWriter, r *http.Request, status int, id string) {
	run, err := s.a.Get(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, run)
}

// decode reads a JSON body of at most MaxBodyBytes into v. It rejects
// unknown fields and trailing data. On failure it writes the problem and
// returns false.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return s.decodeLimit(w, r, v, MaxBodyBytes)
}

// decodeFiles is decode for a body that can hold files. The files can take
// the body up to MaxFilesBodyBytes. Without files, MaxBodyBytes applies.
func (s *server) decodeFiles(w http.ResponseWriter, r *http.Request, v any, files func() int) bool {
	n := &countingReader{r: r.Body}
	r.Body = n
	if !s.decodeLimit(w, r, v, MaxFilesBodyBytes) {
		return false
	}
	if files() == 0 && n.n > MaxBodyBytes {
		problem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", MaxBodyBytes))
		return false
	}
	return true
}

// countingReader counts the bytes that it reads.
type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

// decodeLimit is decode with a limit other than MaxBodyBytes.
func (s *server) decodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("the body holds data after the JSON object")
		}
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		problem(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", limit))
	case errors.Is(err, io.EOF):
		problem(w, r, http.StatusBadRequest, "the body is empty; send a JSON object")
	default:
		problem(w, r, http.StatusBadRequest, "the body is not valid: "+err.Error())
	}
	return false
}

// fail maps a kernel error to a problem document.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, fw.ErrSaturated):
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem(w, r, http.StatusTooManyRequests, "run admission is full; retry after the indicated delay with the same idempotency key")
	case errors.Is(err, fw.ErrStorageUnavailable):
		problem(w, r, http.StatusServiceUnavailable, "storage failed; execution is blocked until the agent restarts; retry with the same idempotency key after recovery")
	case errors.Is(err, fw.ErrAuthzUnavailable):
		// The control plane of the authorization did not answer. A 503 tells
		// the caller to try again, which a 403 would not.
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		problem(w, r, http.StatusServiceUnavailable, unavailableDetail(err))
	case errors.Is(err, fw.ErrForbidden):
		problem(w, r, http.StatusForbidden, forbiddenDetail(err))
	case errors.Is(err, fw.ErrRunNotFound):
		problem(w, r, http.StatusNotFound, "no run has the id "+strconv.Quote(r.PathValue("id")))
	case errors.Is(err, fw.ErrProfileNotFound):
		name := r.PathValue("name")
		if name == "" {
			name = err.Error()
			if i := strings.LastIndex(name, ": "); i >= 0 {
				name = name[i+2:]
			}
		}
		problem(w, r, http.StatusNotFound, "no agent profile has the name "+strconv.Quote(name))
	case errors.Is(err, fw.ErrAgentNotFound):
		problem(w, r, http.StatusNotFound, err.Error())
	case errors.Is(err, fw.ErrInvalidRequest):
		problem(w, r, http.StatusBadRequest, strings.TrimPrefix(err.Error(), fw.ErrInvalidRequest.Error()+": "))
	case errors.Is(err, fw.ErrSpawnLimit), errors.Is(err, fw.ErrRunExists):
		problem(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, fw.ErrNoPendingApproval):
		problem(w, r, http.StatusConflict, err.Error())
	case errors.Is(err, fw.ErrClosed):
		problem(w, r, http.StatusServiceUnavailable, "the agent is shutting down")
	default:
		s.a.Logger().Error("native: request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		problem(w, r, http.StatusInternalServerError, "the request failed; the server log has the cause")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

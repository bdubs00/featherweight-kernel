package fw

// This file is the external decision point (docs/ARCHITECTURE.md section
// 13.5): an MCP tool or a bus subject outside the process that decides who
// can do what (access.external) and which tool calls run (policy.external).
// fw sends the facts and enforces the answer. It makes no decision of its
// own for that part, so the control plane is the single source of truth.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ErrAuthzUnavailable is returned when the external decision point does
// not answer. A surface answers 503, so a caller can tell a refusal from a
// control plane that is down.
var ErrAuthzUnavailable = errors.New("fw: authorization unavailable")

// Defaults of an external decision point.
const (
	DefaultAuthzTool       = "authorize"     // the MCP tool of access.external
	DefaultPolicyTool      = "decide"        // the MCP tool of policy.external
	DefaultDecisionTimeout = 5 * time.Second // the limit of one request
	DecisionVersion        = 1               // the version of the request shape
)

// Kinds of a decision request.
const (
	DecisionAuthz  = "authz"
	DecisionPolicy = "policy"
)

// Bus event types of a decision request and its answer.
const (
	EvDecisionRequested = "fw.decision.requested"
	EvDecisionAnswered  = "fw.decision.answered"
)

// DecisionPoint sends one decision request out of process and returns the
// answer. The request and the answer are JSON objects: AuthzRequest and
// AuthzAnswer, or PolicyRequest and PolicyAnswer. A DecisionPoint that
// implements io.Closer is closed at shutdown, and one that implements
// Prober is checked by fw doctor.
type DecisionPoint interface {
	Decide(ctx context.Context, request json.RawMessage) (json.RawMessage, error)
}

// Prober is a DecisionPoint that can tell whether it is ready, for example
// that the MCP server lists the tool.
type Prober interface {
	Probe(ctx context.Context) error
}

// DecisionPointCtor builds a DecisionPoint for one ExternalPolicyConfig.
// The configuration arrives with its defaults set. The agent gives the
// configuration, the secrets, the bus, and the logger.
type DecisionPointCtor func(ctx context.Context, a *Agent, ext ExternalPolicyConfig) (DecisionPoint, error)

var decisionPoints = struct {
	mu    sync.RWMutex
	ctors map[string]DecisionPointCtor
}{ctors: map[string]DecisionPointCtor{}}

// RegisterDecisionPoint registers the transport of an external decision
// point, for example "mcp". The transport "bus" is built in.
func RegisterDecisionPoint(transport string, c DecisionPointCtor) {
	decisionPoints.mu.Lock()
	defer decisionPoints.mu.Unlock()
	decisionPoints.ctors[transport] = c
}

// Transport names the transport of the configuration: "mcp", "bus", or ""
// when neither or both are set.
func (e ExternalPolicyConfig) Transport() string {
	switch {
	case e.MCP != "" && e.Bus == "":
		return "mcp"
	case e.Bus != "" && e.MCP == "":
		return "bus"
	}
	return ""
}

// String names the decision point for a log line or an error.
func (e ExternalPolicyConfig) String() string {
	switch e.Transport() {
	case "mcp":
		return fmt.Sprintf("the tool %s of the MCP server %s", e.Tool, e.MCP)
	case "bus":
		return "the bus subject " + e.Bus
	}
	return "an external decision point"
}

// withDefaults fills the tool and the timeout.
func (e ExternalPolicyConfig) withDefaults(tool string) ExternalPolicyConfig {
	if e.Tool == "" {
		e.Tool = tool
	}
	if e.Timeout <= 0 {
		e.Timeout = DefaultDecisionTimeout
	}
	return e
}

// ControlServers returns the names of the MCP servers that access.external
// and policy.external use, as the configuration gives them. The MCP tool
// source leaves them out, so the model never sees or calls a decision
// tool. Compare a name with IsControlServer, which matches loosely.
func (c *Config) ControlServers() map[string]bool {
	out := map[string]bool{}
	for _, e := range []*ExternalPolicyConfig{c.Access.External, c.Policy.External} {
		if e != nil && e.MCP != "" {
			out[e.MCP] = true
		}
	}
	return out
}

// IsControlServer reports whether access.external or policy.external names
// the MCP server, with the loose match of SameServerName.
func (c *Config) IsControlServer(name string) bool {
	for n := range c.ControlServers() {
		if SameServerName(n, name) {
			return true
		}
	}
	return false
}

// SameServerName reports whether two names of a tools.mcp entry name the
// same entry: case does not matter, and "-" equals "_", as for the FW_*
// variables.
func SameServerName(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "-", "_"), strings.ReplaceAll(b, "-", "_"))
}

// newDecisionPoint builds the decision point of one configuration.
func (a *Agent) newDecisionPoint(ctx context.Context, key string, ext ExternalPolicyConfig) (DecisionPoint, error) {
	switch ext.Transport() {
	case "bus":
		return newBusDecisionPoint(a, ext), nil
	case "mcp":
		decisionPoints.mu.RLock()
		c, ok := decisionPoints.ctors["mcp"]
		decisionPoints.mu.RUnlock()
		if !ok {
			return nil, fmt.Errorf("fw: %s.mcp needs the MCP driver: import github.com/bdubs00/featherweight-kernel/tools/mcp", key)
		}
		dp, err := c(ctx, a, ext)
		if err != nil {
			return nil, fmt.Errorf("fw: %s: %w", key, err)
		}
		return dp, nil
	}
	return nil, fmt.Errorf("fw: %s needs exactly one of mcp and bus", key)
}

// setupExternal builds the external Authz and Policy that the configuration
// names, unless an option already set them.
func (a *Agent) setupExternal(ctx context.Context, authzSet, policySet bool) error {
	if ext := a.cfg.Access.External; ext != nil && !authzSet {
		e := ext.withDefaults(DefaultAuthzTool)
		dp, err := a.newDecisionPoint(ctx, "access.external", e)
		if err != nil {
			return err
		}
		a.decisionPoints = append(a.decisionPoints, external{use: "access", ext: e, dp: dp})
		a.authz = &externalAuthz{a: a, dp: dp, ext: e, cache: map[string]cachedDecision{}}
	}
	if ext := a.cfg.Policy.External; ext != nil && !policySet {
		e := ext.withDefaults(DefaultPolicyTool)
		dp, err := a.newDecisionPoint(ctx, "policy.external", e)
		if err != nil {
			return err
		}
		a.decisionPoints = append(a.decisionPoints, external{use: "policy", ext: e, dp: dp})
		a.policy = &externalPolicy{a: a, dp: dp, ext: e}
	}
	return nil
}

// external is one decision point of the agent.
type external struct {
	use string // "access" or "policy"
	ext ExternalPolicyConfig
	dp  DecisionPoint
}

// ExternalStatus is the state of one external decision point, for fw
// doctor. Use is "access" or "policy". Err is nil when the decision point
// is ready, or when it cannot tell (Probed is false).
type ExternalStatus struct {
	Use    string
	Target string
	Probed bool
	Err    error
}

// ProbeExternal checks every external decision point of the agent.
func (a *Agent) ProbeExternal(ctx context.Context) []ExternalStatus {
	out := make([]ExternalStatus, 0, len(a.decisionPoints))
	for _, x := range a.decisionPoints {
		st := ExternalStatus{Use: x.use, Target: x.ext.String()}
		if p, ok := x.dp.(Prober); ok {
			pctx, cancel := context.WithTimeout(ctx, x.ext.Timeout)
			st.Probed, st.Err = true, p.Probe(pctx)
			cancel()
		}
		out = append(out, st)
	}
	return out
}

// closeDecisionPoints closes the decision points that hold a connection.
func (a *Agent) closeDecisionPoints() {
	for _, x := range a.decisionPoints {
		if c, ok := x.dp.(io.Closer); ok {
			if err := c.Close(); err != nil {
				a.log.Warn("close decision point", "use", x.use, "error", err)
			}
		}
	}
}

// AuthzRequest is what fw sends to access.external. It asks about one
// principal and one or more checks, and the answer holds one decision per
// check, in the same order.
type AuthzRequest struct {
	Kind      string       `json:"kind"`    // DecisionAuthz
	Version   int          `json:"version"` // DecisionVersion
	Principal Principal    `json:"principal"`
	Checks    []AuthzQuery `json:"checks"`
}

// AuthzQuery is one check: an action and its target. Agent names the agent
// profile. Access is the access block of that profile, as data that the
// decision point can use or ignore. Run is set for an action on a run.
// Member names the fleet member that owns the agent and the run when it is
// another process (section 8.6); Access and the lineage then come from the
// record of that member.
type AuthzQuery struct {
	Action string    `json:"action"`
	Member string    `json:"member,omitempty"`
	Agent  string    `json:"agent,omitempty"`
	Access *Access   `json:"access,omitempty"`
	Run    *RunFacts `json:"run,omitempty"`
}

// RunFacts describes a run to a decision point. Principal is the subject
// that started the run. Lineage lists the ancestors of a child run, the
// parent first, so the decision point can grant a child from its parent.
type RunFacts struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Principal string   `json:"principal,omitempty"`
	State     RunState `json:"state,omitempty"`
	Origin    string   `json:"origin,omitempty"`
	Lineage   []RunRef `json:"lineage,omitempty"`
}

// RunRef names one ancestor of a run.
type RunRef struct {
	ID        string `json:"id"`
	Agent     string `json:"agent"`
	Principal string `json:"principal,omitempty"`
}

// AuthzAnswer is what access.external returns: one decision per check.
type AuthzAnswer struct {
	Decisions []AuthzDecision `json:"decisions"`
}

// AuthzDecision allows or refuses one check. Reason reaches the caller in
// the 403 problem. TTLMs lets fw reuse the decision for that many
// milliseconds; zero means no reuse, so a revocation takes effect at once.
type AuthzDecision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
	TTLMs  int64  `json:"ttlMs,omitempty"`
}

// PolicyRequest is what fw sends to policy.external before a tool call.
type PolicyRequest struct {
	Kind    string          `json:"kind"`    // DecisionPolicy
	Version int             `json:"version"` // DecisionVersion
	Tool    ToolFacts       `json:"tool"`
	CallID  string          `json:"callId"`
	Input   json.RawMessage `json:"input"`
	// Resolved is the input with each target named as the tool resolves
	// it, such as the real path of a file (see Resolver).
	Resolved json.RawMessage `json:"resolved,omitempty"`
	Run      *RunFacts       `json:"run,omitempty"`
}

// ToolFacts describes the tool of a call: its name, where it comes from,
// and its flags.
type ToolFacts struct {
	Name         string `json:"name"`
	Source       string `json:"source,omitempty"`
	ReadOnly     bool   `json:"readOnly,omitempty"`
	Idempotent   bool   `json:"idempotent,omitempty"`
	Destructive  bool   `json:"destructive,omitempty"`
	ParallelSafe bool   `json:"parallelSafe,omitempty"`
	Trusted      bool   `json:"trusted,omitempty"`
}

// PolicyAnswer is what policy.external returns. Verdict is "allow",
// "deny", or "ask". "ask" asks the caller for an approval.
type PolicyAnswer struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
}

// runFacts describes a run with its lineage.
func (a *Agent) runFacts(r *Run) *RunFacts {
	if r == nil {
		return nil
	}
	f := &RunFacts{ID: r.ID, Agent: r.Agent, Principal: r.Principal, State: r.State, Origin: r.Origin}
	for cur, depth := r, 0; cur.Parent != "" && a.k != nil && depth < 64; depth++ {
		rs, err := a.k.lookup(cur.Parent)
		if err != nil {
			break
		}
		cur = rs.snapshot()
		f.Lineage = append(f.Lineage, RunRef{ID: cur.ID, Agent: cur.Agent, Principal: cur.Principal})
	}
	return f
}

// externalAuthz asks access.external about every check. It decides about a
// child run from the lineage in the request, so Authorize does not ask
// again for the ancestors.
type externalAuthz struct {
	a   *Agent
	dp  DecisionPoint
	ext ExternalPolicyConfig

	mu    sync.Mutex
	cache map[string]cachedDecision
}

type cachedDecision struct {
	err     error
	expires time.Time
}

// maxCachedDecisions bounds the cache of decisions.
const maxCachedDecisions = 4096

func (*externalAuthz) decidesLineage() {}

// Allow implements Authz.
func (z *externalAuthz) Allow(ctx context.Context, p *Principal, action string, t Target) error {
	return z.AllowAll(ctx, p, []AuthzCheck{{Action: action, Target: t}})[0]
}

// AllowAll implements BatchAuthz. It answers from the cache when it can and
// sends the other checks in one request.
func (z *externalAuthz) AllowAll(ctx context.Context, p *Principal, checks []AuthzCheck) []error {
	out := make([]error, len(checks))
	if p == nil {
		return out
	}
	now := z.a.now()
	// The facts come from the run cache and the profiles, so they are read
	// before the lock of the decision cache.
	queries := make([]AuthzQuery, len(checks))
	keys := make([]string, len(checks))
	for i, c := range checks {
		queries[i] = z.query(c)
		keys[i] = cacheKey(p, queries[i])
	}
	var miss []int
	z.mu.Lock()
	for i := range checks {
		if d, ok := z.cache[keys[i]]; ok && now.Before(d.expires) {
			out[i] = d.err
			continue
		}
		miss = append(miss, i)
	}
	z.mu.Unlock()
	if len(miss) == 0 {
		return out
	}
	req := AuthzRequest{Kind: DecisionAuthz, Version: DecisionVersion, Principal: *p}
	for _, i := range miss {
		req.Checks = append(req.Checks, queries[i])
	}
	ans, err := z.ask(ctx, req)
	if err != nil {
		z.a.log.Warn("external authorization failed", "decisionPoint", z.ext.String(), "error", err)
		for _, i := range miss {
			out[i] = fmt.Errorf("%w: %s did not answer: %v", ErrAuthzUnavailable, z.ext, err)
		}
		return out
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	for j, i := range miss {
		d := ans.Decisions[j]
		if !d.Allow {
			reason := strings.TrimSpace(d.Reason)
			if reason == "" {
				reason = "the control plane refused it"
			}
			out[i] = fmt.Errorf("%w: %s %s: %s", ErrForbidden, p.Subject, checks[i].Action, reason)
		}
		if d.TTLMs > 0 {
			if len(z.cache) >= maxCachedDecisions {
				z.sweep(now)
			}
			z.cache[keys[i]] = cachedDecision{err: out[i], expires: now.Add(time.Duration(d.TTLMs) * time.Millisecond)}
		}
	}
	return out
}

// query turns a check into the facts that the request carries.
func (z *externalAuthz) query(c AuthzCheck) AuthzQuery {
	agent := c.Target.Agent
	if agent == "" && c.Target.Run != nil {
		agent = c.Target.Run.Agent
	}
	if c.Target.Member != "" {
		q := AuthzQuery{Action: c.Action, Member: c.Target.Member, Agent: agent, Access: c.Target.Access}
		if r := c.Target.Run; r != nil {
			q.Run = &RunFacts{ID: r.ID, Agent: r.Agent, Principal: r.Principal, State: r.State, Origin: r.Origin, Lineage: c.Target.Lineage}
		}
		return q
	}
	q := AuthzQuery{Action: c.Action, Agent: agent, Run: z.a.runFacts(c.Target.Run)}
	if agent != "" {
		if prof, ok := z.a.ProfileNamed(agent); ok && prof.Access != nil {
			acc := *prof.Access
			q.Access = &acc
		}
	}
	return q
}

// ask sends one request and checks the shape of the answer.
func (z *externalAuthz) ask(ctx context.Context, req AuthzRequest) (AuthzAnswer, error) {
	var ans AuthzAnswer
	body, err := json.Marshal(req)
	if err != nil {
		return ans, err
	}
	ctx, cancel := context.WithTimeout(ctx, z.ext.Timeout)
	defer cancel()
	raw, err := z.dp.Decide(ctx, body)
	if err != nil {
		return ans, err
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return ans, fmt.Errorf("the answer is not an AuthzAnswer: %w", err)
	}
	if len(ans.Decisions) != len(req.Checks) {
		return ans, fmt.Errorf("the answer holds %d decisions for %d checks", len(ans.Decisions), len(req.Checks))
	}
	return ans, nil
}

// sweep drops expired decisions, and every decision when none expired. The
// caller holds z.mu.
func (z *externalAuthz) sweep(now time.Time) {
	for k, d := range z.cache {
		if !now.Before(d.expires) {
			delete(z.cache, k)
		}
	}
	if len(z.cache) >= maxCachedDecisions {
		clear(z.cache)
	}
}

// cacheKey identifies a decision by every fact that the request carries.
func cacheKey(p *Principal, q AuthzQuery) string {
	b, _ := json.Marshal(struct {
		P *Principal `json:"p"`
		Q AuthzQuery `json:"q"`
	}{p, q})
	return string(b)
}

// externalPolicy asks policy.external about every tool call. A decision
// point that does not answer denies the call.
type externalPolicy struct {
	a   *Agent
	dp  DecisionPoint
	ext ExternalPolicyConfig
}

// Decide implements Policy.
func (x *externalPolicy) Decide(ctx context.Context, call ToolCall) Decision {
	req := PolicyRequest{
		Kind:    DecisionPolicy,
		Version: DecisionVersion,
		Tool: ToolFacts{
			Name: call.Spec.Name, Source: call.Spec.Source, ReadOnly: call.Spec.ReadOnly, Idempotent: call.Spec.Idempotent,
			Destructive: call.Spec.Destructive, ParallelSafe: call.Spec.ParallelSafe, Trusted: call.Spec.Trusted,
		},
		CallID:   call.CallID,
		Input:    validJSON(call.Input),
		Resolved: call.Resolved,
	}
	if call.RunID != "" && x.a.k != nil {
		if rs, err := x.a.k.lookup(call.RunID); err == nil {
			req.Run = x.a.runFacts(rs.snapshot())
		}
	}
	unavailable := func(err error) Decision {
		x.a.log.Warn("external policy failed", "decisionPoint", x.ext.String(), "tool", call.Spec.Name, "error", err)
		return Decision{Verdict: VerdictDeny, Reason: fmt.Sprintf("external policy: %s did not answer, so the call is denied: %v", x.ext, err)}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return unavailable(err)
	}
	cctx, cancel := context.WithTimeout(ctx, x.ext.Timeout)
	defer cancel()
	raw, err := x.dp.Decide(cctx, body)
	if err != nil {
		return unavailable(err)
	}
	var ans PolicyAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return unavailable(fmt.Errorf("the answer is not a PolicyAnswer: %w", err))
	}
	reason := strings.TrimSpace(ans.Reason)
	if reason == "" {
		reason = "external policy"
	} else {
		reason = "external policy: " + reason
	}
	switch strings.ToLower(strings.TrimSpace(ans.Verdict)) {
	case "allow":
		return Decision{Verdict: VerdictAllow, Reason: reason}
	case "ask":
		return Decision{Verdict: VerdictAsk, Reason: reason}
	case "deny":
		return Decision{Verdict: VerdictDeny, Reason: reason}
	}
	return unavailable(fmt.Errorf("the verdict %q is not allow, deny, or ask", ans.Verdict))
}

// busDecisionPoint sends a request as a fw.decision.requested event to a
// bus subject and waits for the fw.decision.answered event on a reply
// subject of this process. Any process that subscribes to the subject can
// answer.
type busDecisionPoint struct {
	bus     Bus
	subject string
	reply   string

	mu      sync.Mutex
	stop    func()
	waiting map[string]chan json.RawMessage
}

// busRequest is the data of fw.decision.requested.
type busRequest struct {
	ReplyTo string          `json:"replyTo"`
	Request json.RawMessage `json:"request"`
}

// busAnswer is the data of fw.decision.answered.
type busAnswer struct {
	InReplyTo string          `json:"inReplyTo"`
	Answer    json.RawMessage `json:"answer"`
}

func newBusDecisionPoint(a *Agent, ext ExternalPolicyConfig) *busDecisionPoint {
	return &busDecisionPoint{
		bus:     a.bus,
		subject: ext.Bus,
		reply:   ext.Bus + ".reply." + strings.ToLower(NewID()),
		waiting: map[string]chan json.RawMessage{},
	}
}

// Decide implements DecisionPoint.
func (b *busDecisionPoint) Decide(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	if b.bus == nil {
		return nil, errors.New("fw: bus decision point: no bus")
	}
	if err := b.listen(); err != nil {
		return nil, err
	}
	e, err := NewEvent(NewID(), EvDecisionRequested, busRequest{ReplyTo: b.reply, Request: request})
	if err != nil {
		return nil, err
	}
	ch := make(chan json.RawMessage, 1)
	b.mu.Lock()
	b.waiting[e.ID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiting, e.ID)
		b.mu.Unlock()
	}()
	if err := b.bus.Publish(ctx, b.subject, e); err != nil {
		return nil, fmt.Errorf("fw: bus decision point: publish to %s: %w", b.subject, err)
	}
	select {
	case ans := <-ch:
		return ans, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("fw: bus decision point: no answer on %s: %w", b.reply, context.Cause(ctx))
	}
}

// listen subscribes to the reply subject once.
func (b *busDecisionPoint) listen() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		return nil
	}
	stop, err := b.bus.Subscribe(context.Background(), b.reply, func(e Event) error {
		var d busAnswer
		if e.Type != EvDecisionAnswered || e.Decode(&d) != nil {
			return nil
		}
		b.mu.Lock()
		ch := b.waiting[d.InReplyTo]
		b.mu.Unlock()
		if ch != nil {
			select {
			case ch <- d.Answer:
			default:
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fw: bus decision point: subscribe to %s: %w", b.reply, err)
	}
	b.stop = stop
	return nil
}

// Close implements io.Closer.
func (b *busDecisionPoint) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	return nil
}

// AnswerDecision builds the fw.decision.answered event for a request event,
// for a Go program that answers decisions on the bus. Publish it to the
// subject that ReplyTo returns.
func AnswerDecision(request Event, answer any) (subject string, e Event, err error) {
	var d busRequest
	if err := request.Decode(&d); err != nil {
		return "", Event{}, fmt.Errorf("fw: decision request %s: %w", request.ID, err)
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return "", Event{}, fmt.Errorf("fw: decision answer: %w", err)
	}
	e, err = NewEvent(request.Subject, EvDecisionAnswered, busAnswer{InReplyTo: request.ID, Answer: raw})
	return d.ReplyTo, e, err
}

// DecisionRequestOf returns the request inside a fw.decision.requested
// event: an AuthzRequest or a PolicyRequest, told apart by its kind field.
func DecisionRequestOf(e Event) (json.RawMessage, error) {
	var d busRequest
	if err := e.Decode(&d); err != nil {
		return nil, fmt.Errorf("fw: decision request %s: %w", e.ID, err)
	}
	return d.Request, nil
}

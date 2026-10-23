package fw

// This file is the primitive of a connection: a tool call that acts for a
// person pauses until a credential of that person arrives from outside
// (docs/ARCHITECTURE.md section 7.11). The kernel keeps no grant, no
// refresh token, and no OAuth flow. A linked package, the workbench, keeps
// the grants of people and gives the kernel their access tokens through
// PersonTokens. No token enters a ledger event, a log line, or an error
// message.

import (
	"context"
	"errors"
	"fmt"
)

// Errors of connections.
var (
	// ErrConnectSkipped means that the person chose not to connect the
	// service, so the call that needed it does not run.
	ErrConnectSkipped = errors.New("fw: the person did not connect the service")
	// ErrNotConnected means that the person never connected the service,
	// or disconnected it.
	ErrNotConnected = errors.New("fw: the person has not connected the service")
	// ErrConnectionExpired means that the authorization server refused the
	// grant of the person, so the person connects again.
	ErrConnectionExpired = errors.New("fw: the service refused the connection of the person")
)

// PersonTokens gives a tool source that acts for a person the access token
// of that person at one service. A linked package sets one with
// Agent.SetPersonTokens. The workbench does: it keeps the grants of people
// and runs the OAuth flows (the workbench architecture, section 11).
// Without one, a tool source that acts for a person tells the model that
// this process keeps no connections of people.
//
// Token returns an access token of the person subject at the service
// server, the name of the tools.mcp entry. It returns an error that wraps
// ErrNotConnected when the person never connected the service, and one
// that wraps ErrConnectionExpired when the authorization server refused
// the grant, so the person connects again. Reject reports that the service
// refused the token used, so the next Token gets a new one. Forget drops
// the grant of the person at the service, because the service kept
// refusing its tokens, so the next Token returns ErrNotConnected.
type PersonTokens interface {
	Token(ctx context.Context, subject, server string) (string, error)
	Reject(ctx context.Context, subject, server, used string)
	Forget(ctx context.Context, subject, server string)
}

// PersonTokens returns the tokens of people that a linked package set, or
// nil when none did.
func (a *Agent) PersonTokens() PersonTokens {
	a.tokMu.RLock()
	defer a.tokMu.RUnlock()
	return a.personTokens
}

// SetPersonTokens sets the tokens of people that the tool sources use. A
// linked package calls it when it starts, before the first run. Nil
// removes them.
func (a *Agent) SetPersonTokens(p PersonTokens) {
	a.tokMu.Lock()
	defer a.tokMu.Unlock()
	a.personTokens = p
}

// Reasons of a ConnectRequest.
const (
	ConnectMissing = "missing" // the person never connected the service
	ConnectExpired = "expired" // the server refused the grant, so the person signs in again
)

// ConnectRequest pauses a tool call until the person of the run connects a
// service. It travels as an approval with Connect set, so the run moves to
// input-required and every surface shows it. Only the end of a connection
// allows it: the package that keeps the connections calls Agent.Connected,
// which decides every pending connect of the person for the services that
// are now connected. A person can deny it, which skips the service: the
// call fails with ErrConnectSkipped. A decision that allows it from a
// surface is refused, because a click is not a grant.
type ConnectRequest struct {
	Server string `json:"server"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason,omitempty"` // ConnectMissing or ConnectExpired
}

// Message is the sentence that a surface shows for the request.
func (r ConnectRequest) Message() string {
	title := r.Title
	if title == "" {
		title = r.Server
	}
	if r.Reason == ConnectExpired {
		return "Sign in to " + title + " again, so the agent can go on acting as you."
	}
	return "Connect " + title + ", so the agent can act as you there."
}

// Connecter pauses a tool call until the person connects a service.
type Connecter func(ctx context.Context, req ConnectRequest) error

type connecterKey struct{}

// WithConnecter stores a connecter in the context. The kernel sets one for
// every tool call.
func WithConnecter(ctx context.Context, fn Connecter) context.Context {
	return context.WithValue(ctx, connecterKey{}, fn)
}

// RequestConnect pauses the tool call in ctx until the person of the run
// connects req.Server. It returns nil once the person connected,
// ErrConnectSkipped when the person skipped the service, and ErrNoAsker
// outside a tool call. The caller checks that the run has a person
// (RunInfo.PrincipalKind is KindUser) first, because nobody else can
// connect.
func RequestConnect(ctx context.Context, req ConnectRequest) error {
	fn, ok := ctx.Value(connecterKey{}).(Connecter)
	if !ok || fn == nil {
		return ErrNoAsker
	}
	return fn(ctx, req)
}

// Connected lets every paused call of the person subject go on whose
// service is one of servers. The package that keeps the connections calls
// it when a connection of the person ends. The decision comes from the
// process, which is the only decider that a connect accepts. It returns
// the number of runs that went on.
func (a *Agent) Connected(subject string, servers ...string) int {
	n := 0
	for _, rs := range a.k.all() {
		rs.mu.Lock()
		p, principal := rs.run.Pending, rs.queued.Principal
		rs.mu.Unlock()
		if p == nil || p.Connect == nil || principal != subject {
			continue
		}
		ok := false
		for _, s := range servers {
			if SameServerName(s, p.Connect.Server) {
				ok = true
				break
			}
		}
		if !ok {
			continue
		}
		err := a.Decide(context.Background(), rs.id, ApprovalDecision{ApprovalID: p.ID, Decision: "allow", Decider: subject, DeciderKind: ActorPerson, Note: "connected"})
		if err == nil {
			n++
		}
	}
	if n > 0 {
		a.log.Info("connection resumed paused calls", "subject", subject, "servers", fmt.Sprint(servers), "runs", n)
	}
	return n
}

func connectTitle(r *ConnectRequest) string {
	if r.Title != "" {
		return r.Title
	}
	return r.Server
}

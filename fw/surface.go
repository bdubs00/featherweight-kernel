package fw

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Surface mounts routes on the shared mux.
type Surface interface {
	Mount(mux *http.ServeMux, a *Agent) error
}

// SurfaceFunc adapts a function to Surface.
type SurfaceFunc func(mux *http.ServeMux, a *Agent) error

// Mount implements Surface.
func (f SurfaceFunc) Mount(mux *http.ServeMux, a *Agent) error { return f(mux, a) }

// Kinds of a principal.
const (
	KindUser      = "user"      // a person, from an OIDC token
	KindWorkload  = "workload"  // a workload, from a SPIFFE identity
	KindToken     = "token"     // the static bearer token of the process
	KindLocal     = "local"     // the local user of the process, over stdio
	KindAnonymous = "anonymous" // no credential, when auth.mode is none
)

// Principal is an authenticated caller.
//
// Subject is the stable identity: an OIDC subject, a SPIFFE ID such as
// spiffe://example.org/ns/prod/sa/reviewer, or a fixed word for the static
// token. Name is what a person reads, such as an email address. Groups come
// from the groups claim of a token. Issuer names who vouched for the
// principal. An access entry (see Access) matches a principal through
// PrincipalMatches.
type Principal struct {
	Subject string   `json:"subject"`
	Name    string   `json:"name,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Issuer  string   `json:"issuer,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Scopes  []string `json:"scopes,omitempty"`
}

// PrincipalMatches reports whether an access entry names a principal.
//
// The entry "*" matches every principal. "group:<glob>" matches a group of
// the principal. "kind:<kind>" matches the kind, for example kind:workload.
// "issuer:<glob>" matches the issuer. Any other entry is a glob over the
// subject and over the name, so alice@example.com, spiffe://td/ns/prod/*,
// and local:* all work. In a glob, "*" matches any text, slashes included.
func PrincipalMatches(entry string, p *Principal) bool {
	entry = strings.TrimSpace(entry)
	if entry == "" || p == nil {
		return false
	}
	if entry == "*" {
		return true
	}
	if g, ok := strings.CutPrefix(entry, "group:"); ok {
		for _, have := range p.Groups {
			if wildcardMatch(g, have) {
				return true
			}
		}
		return false
	}
	if k, ok := strings.CutPrefix(entry, "kind:"); ok {
		return strings.EqualFold(k, p.Kind)
	}
	if iss, ok := strings.CutPrefix(entry, "issuer:"); ok {
		return wildcardMatch(iss, p.Issuer)
	}
	entry = strings.TrimPrefix(entry, "user:")
	if p.Subject != "" && wildcardMatch(entry, p.Subject) {
		return true
	}
	return p.Name != "" && wildcardMatch(entry, p.Name)
}

// MatchesAny reports whether any entry names the principal.
func MatchesAny(entries []string, p *Principal) bool {
	for _, e := range entries {
		if PrincipalMatches(e, p) {
			return true
		}
	}
	return false
}

// ErrUnauthorized is returned when a request carries no valid credential.
// An Authn wraps it with a reason in words and no token material, such as
// "fw: unauthorized: oidc: token expired".
var ErrUnauthorized = errors.New("fw: unauthorized")

// refusalReason returns the reason of an error of Authenticate without the
// words of ErrUnauthorized, such as "oidc: token expired", or "" when the
// error names none.
func refusalReason(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	s = strings.ReplaceAll(s, ErrUnauthorized.Error()+": ", "")
	s = strings.ReplaceAll(s, ": "+ErrUnauthorized.Error(), "")
	if s == ErrUnauthorized.Error() {
		return ""
	}
	return strings.TrimSpace(s)
}

// Authn authenticates HTTP requests for every surface.
type Authn interface {
	// Authenticate returns the principal, or ErrUnauthorized.
	Authenticate(r *http.Request) (*Principal, error)
	// Challenge writes the 401 response for a failed request.
	Challenge(w http.ResponseWriter, r *http.Request)
}

// ChainAuthn tries several authenticators in order and accepts the first
// principal. The configuration builds one from auth.mode with several
// modes separated by commas, for example "oidc,spiffe,token". The first
// authenticator writes the challenge. A refusal names the reason of each
// authenticator, such as "oidc: token expired; token: the bearer token is
// not the static token".
type ChainAuthn []Authn

// Authenticate implements Authn.
func (c ChainAuthn) Authenticate(r *http.Request) (*Principal, error) {
	var reasons []string
	for _, a := range c {
		p, err := a.Authenticate(r)
		if err == nil && p != nil {
			return p, nil
		}
		if why := refusalReason(err); why != "" && !slices.Contains(reasons, why) {
			reasons = append(reasons, why)
		}
	}
	if len(reasons) == 0 {
		return nil, ErrUnauthorized
	}
	return nil, fmt.Errorf("%w: %s", ErrUnauthorized, strings.Join(reasons, "; "))
}

// Challenge implements Authn.
func (c ChainAuthn) Challenge(w http.ResponseWriter, r *http.Request) {
	if len(c) == 0 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	c[0].Challenge(w, r)
}

// NoAuthn accepts every request as the anonymous principal.
type NoAuthn struct{}

// Authenticate implements Authn.
func (NoAuthn) Authenticate(*http.Request) (*Principal, error) {
	return &Principal{Subject: KindAnonymous, Kind: KindAnonymous}, nil
}

// Challenge implements Authn.
func (NoAuthn) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusUnauthorized)
}

// TokenAuthn accepts one static bearer token.
type TokenAuthn struct {
	Token string
}

// Authenticate implements Authn.
func (t TokenAuthn) Authenticate(r *http.Request) (*Principal, error) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return nil, fmt.Errorf("%w: token: no bearer token", ErrUnauthorized)
	}
	got := strings.TrimSpace(h[len(prefix):])
	if subtle.ConstantTimeCompare([]byte(got), []byte(t.Token)) != 1 {
		return nil, fmt.Errorf("%w: token: the bearer token is not the static token", ErrUnauthorized)
	}
	return &Principal{Subject: KindToken, Kind: KindToken}, nil
}

// Challenge implements Authn.
func (TokenAuthn) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw"`)
	w.WriteHeader(http.StatusUnauthorized)
}

type principalKey struct{}

// WithPrincipal stores a principal in the context. The kernel authorizes
// every call that carries a principal, and trusts a call that carries none,
// because such a call comes from the process itself.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in the context, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// RequireAuth wraps a handler with the agent's Authn. It stores the
// principal in the request context, and the bearer credential of the
// request too (see WithCredential). A refused request gets the challenge
// of the Authn, and the log a line about it (logRefusal).
func RequireAuth(a *Agent, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authn().Authenticate(r)
		if err != nil {
			a.logRefusal(r, err)
			a.Authn().Challenge(w, r)
			return
		}
		ctx := WithPrincipal(r.Context(), p)
		if c := bearerHeader(r); c != "" {
			ctx = WithCredential(ctx, c)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type credentialKey struct{}

// WithCredential stores the bearer credential of a request in the context:
// the value of its Authorization header. A linked package that sends a
// request on behalf of the caller to another process, such as the fleet
// of the workbench, reads it. RequireAuth sets it. It never enters a ledger
// event or a log line.
func WithCredential(ctx context.Context, authorization string) context.Context {
	return context.WithValue(ctx, credentialKey{}, authorization)
}

// CredentialFrom returns the bearer credential in the context, or "".
func CredentialFrom(ctx context.Context) string {
	s, _ := ctx.Value(credentialKey{}).(string)
	return s
}

// bearerHeader returns the Authorization header of r when it holds a bearer
// token, else "".
func bearerHeader(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(tok) == "" {
		return ""
	}
	return h
}

// refusalEvery is the shortest time between two log lines of refused
// requests with the same method, route, and reason. A client that loops on
// a refusal writes one line a minute, and that line counts the refusals
// that the log left out before it.
const refusalEvery = time.Minute

// maxRefusalKeys bounds the methods, routes, and reasons that the log of
// refusals keeps. The routes of a process and the reasons of its Authn are
// few, so the bound only guards the memory.
const maxRefusalKeys = 512

// refusalLog bounds the log lines of refused requests (logRefusal). The
// zero value is ready.
type refusalLog struct {
	mu   sync.Mutex
	seen map[string]*refusalMark
}

// refusalMark is the time of the last line of one key and the refusals of
// the key that the log left out since that line.
type refusalMark struct {
	at      time.Time
	skipped int
}

// note reports whether a refusal of key gets a line at now, and how many
// refusals of key the log left out since the last line.
func (l *refusalLog) note(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]*refusalMark{}
	}
	m, ok := l.seen[key]
	if ok && now.Sub(m.at) < refusalEvery {
		m.skipped++
		return false, 0
	}
	if !ok && len(l.seen) >= maxRefusalKeys {
		for k, old := range l.seen {
			if now.Sub(old.at) >= refusalEvery {
				delete(l.seen, k)
			}
		}
		if len(l.seen) >= maxRefusalKeys {
			return false, 0
		}
	}
	skipped := 0
	if ok {
		skipped = m.skipped
	}
	l.seen[key] = &refusalMark{at: now}
	return true, skipped
}

// logRefusal writes the line of a request that RequireAuth refused, at
// level info: the method, the route pattern, the path, the reason, and
// the node of the process, so the log of several replicas says which one
// refused. The reason is "no credential" for a request without an
// Authorization header and without a client certificate, else the reason
// of the Authn, such as "oidc: token expired". No token and no other value
// of a header enters the line. Per method, route, and reason, at most one
// line goes out per refusalEvery, and the next line counts the refusals
// that the log left out in skipped.
func (a *Agent) logRefusal(r *http.Request, err error) {
	reason := refusalReason(err)
	if r.Header.Get("Authorization") == "" && (r.TLS == nil || len(r.TLS.PeerCertificates) == 0) {
		reason = "no credential"
	}
	if reason == "" {
		reason = "the credential was refused"
	}
	// The pattern of the mux, such as GET /v1/runs/{id}, without its method.
	route := r.Pattern
	if _, rest, ok := strings.Cut(route, " "); ok {
		route = strings.TrimSpace(rest)
	}
	ok, skipped := a.refusals.note(r.Method+" "+route+"\n"+reason, a.clock.Now())
	if !ok {
		return
	}
	attrs := []any{"method", r.Method, "route", route, "path", r.URL.Path, "reason", reason, "node", a.node}
	if skipped > 0 {
		attrs = append(attrs, "skipped", skipped)
	}
	a.log.Info("request refused without a valid credential", attrs...)
}

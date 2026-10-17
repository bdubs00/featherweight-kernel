package fw

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
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
var ErrUnauthorized = errors.New("fw: unauthorized")

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
// authenticator writes the challenge.
type ChainAuthn []Authn

// Authenticate implements Authn.
func (c ChainAuthn) Authenticate(r *http.Request) (*Principal, error) {
	for _, a := range c {
		if p, err := a.Authenticate(r); err == nil && p != nil {
			return p, nil
		}
	}
	return nil, ErrUnauthorized
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
		return nil, ErrUnauthorized
	}
	got := strings.TrimSpace(h[len(prefix):])
	if subtle.ConstantTimeCompare([]byte(got), []byte(t.Token)) != 1 {
		return nil, ErrUnauthorized
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
// request too (see WithCredential).
func RequireAuth(a *Agent, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authn().Authenticate(r)
		if err != nil {
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

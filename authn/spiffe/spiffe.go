// Package spiffe authenticates workloads by their SPIFFE identity and serves
// the listener over TLS with the X.509-SVID of this workload.
//
// A SPIFFE ID such as spiffe://example.org/ns/prod/sa/reviewer names one
// workload: a service, a job, or another agent. The package registers two
// drivers. The authentication mode "spiffe" accepts a JWT-SVID as a bearer
// token, and an X.509-SVID as a client certificate when auth.spiffe.x509 is
// set. The TLS provider "spiffe" serves the listener with the X.509-SVID of
// this workload and verifies client certificates against the trust bundle.
//
// # Two setups
//
// Inside a SPIRE or google/ax cluster, every Task has a Workload API socket.
// Set auth.spiffe.trustDomain and leave auth.spiffe.bundle empty. The driver
// reads the bundles, and the certificate of this workload, from the socket
// that auth.spiffe.socket or SPIFFE_ENDPOINT_SOCKET names, and keeps them
// current. A socket that does not answer at start does not stop the process:
// the driver logs one warning, refuses every request until the socket
// answers, and then serves.
//
//	auth:
//	  mode: spiffe
//	  spiffe:
//	    trustDomain: example.org
//	    x509: true
//	tls:
//	  provider: spiffe
//
// A validator with no agent, for example a service outside the cluster that
// only checks tokens, sets auth.spiffe.bundle to a file path or an https URL
// of the trust bundle in JWKS form. The driver reads the file again when its
// modification time changes, and fetches the URL every five minutes, or
// sooner when the bundle gives a shorter refresh hint. With a bundle, the
// TLS provider needs tls.cert and tls.key for the server certificate,
// because a bundle holds no certificate for this workload.
//
//	auth:
//	  mode: spiffe
//	  spiffe:
//	    trustDomain: example.org
//	    bundle: /etc/fw/bundle.json
//	    audience: [https://reviewer.example.org]
//	    x509: true
//	tls:
//	  provider: spiffe
//	  cert: /etc/fw/tls.crt
//	  key: /etc/fw/tls.key
//
// # Keys and variables
//
//	auth.spiffe.trustDomain  FW_AUTH_SPIFFE_TRUST_DOMAIN  the trust domain of accepted identities; required
//	auth.spiffe.bundle       FW_AUTH_SPIFFE_BUNDLE        a bundle file or URL; default: the Workload API
//	auth.spiffe.socket       FW_AUTH_SPIFFE_SOCKET        the Workload API address; default: SPIFFE_ENDPOINT_SOCKET
//	auth.spiffe.audience     FW_AUTH_SPIFFE_AUDIENCE      accepted JWT-SVID audiences, comma separated;
//	                                                      default: mcp.auth.audience and a2a.url, else any audience
//	auth.spiffe.x509         FW_AUTH_SPIFFE_X509          accept X.509-SVID client certificates; default: false
//	tls.provider             FW_TLS_PROVIDER              "spiffe" selects this TLS provider
//	tls.cert, tls.key        FW_TLS_CERT, FW_TLS_KEY      the server certificate, needed with a bundle file or URL
//	tls.clientAuth           FW_TLS_CLIENT_AUTH           none, request, or require;
//	                                                      default: request when auth.spiffe.x509 is set, else none
//
// With tls.clientAuth request, a client that presents a certificate must
// present a valid X.509-SVID of the trust domain, and a client that presents
// none goes on to bearer authentication. With require, every client must
// present one.
//
// # Principals
//
// A workload becomes a principal of kind "workload". The subject is the
// SPIFFE ID, the name is its path without the leading slash, and the issuer
// is the trust domain as a SPIFFE ID. Groups stay empty: an access entry is a
// glob over the subject, so one entry names every workload under a path.
// This gives every workload under ns/prod the operator role on one agent:
//
//	agents:
//	  reviewer:
//	    access:
//	      owners: [alice@example.com]
//	      operators: ["spiffe://example.org/ns/prod/*"]
//
// # Calling an MCP server as this workload
//
// An MCP server entry with auth.spiffe sends a JWT-SVID of this workload as
// the bearer token on every request, fetched from the Workload API for the
// audience named, or for the endpoint URL when no audience is named:
//
//	tools:
//	  mcp:
//	    - name: git
//	      endpoint: https://git-mcp.example.org/mcp
//	      auth: {spiffe: {audience: "git-mcp"}}
package spiffe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/jwtsvid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
)

func init() {
	fw.RegisterAuthn("spiffe", New)
	fw.RegisterTLSProvider("spiffe", NewTLS)
}

// Authn is the spiffe authentication mode. It accepts a JWT-SVID bearer
// token of the trust domain, and an X.509-SVID client certificate when
// configured. It implements fw.Authn.
type Authn struct {
	td        spiffeid.TrustDomain
	audiences []string // accepted JWT-SVID audiences; empty accepts any
	x509      bool
	src       *source
	log       *slog.Logger
}

// New builds the spiffe authentication mode from cfg.Auth.SPIFFE. The
// bundle source opens in the background, so a Workload API or a bundle URL
// that does not answer yet does not fail the call. ctx bounds the life of
// the background work.
func New(ctx context.Context, cfg *fw.Config, _ fw.Secrets) (fw.Authn, error) {
	c := cfg.Auth.SPIFFE
	if c == nil {
		c = &fw.SPIFFEConfig{}
	}
	td, err := trustDomain(c.TrustDomain)
	if err != nil {
		return nil, err
	}
	log := slog.Default().With("component", "spiffe")
	src, err := openSource(ctx, td, c.Bundle, c.Socket, true, c.X509, log)
	if err != nil {
		return nil, err
	}
	aud := audiences(cfg)
	if len(aud) == 0 {
		log.Warn("auth.spiffe.audience is empty and neither mcp.auth.audience nor a2a.url is set; a JWT-SVID for any audience is accepted")
	}
	return &Authn{td: td, audiences: aud, x509: c.X509, src: src, log: log}, nil
}

// trustDomain parses the configured trust domain.
func trustDomain(s string) (spiffeid.TrustDomain, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return spiffeid.TrustDomain{}, errors.New("spiffe: auth.spiffe.trustDomain is required, for example example.org")
	}
	td, err := spiffeid.TrustDomainFromString(s)
	if err != nil {
		return spiffeid.TrustDomain{}, fmt.Errorf("spiffe: auth.spiffe.trustDomain %q: %w", s, err)
	}
	return td, nil
}

// audiences returns the accepted JWT-SVID audiences: auth.spiffe.audience,
// else mcp.auth.audience and a2a.url, else none, which accepts any.
func audiences(cfg *fw.Config) []string {
	var out []string
	if cfg.Auth.SPIFFE != nil {
		for _, a := range cfg.Auth.SPIFFE.Audience {
			if a = strings.TrimSpace(a); a != "" {
				out = append(out, a)
			}
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, a := range []string{cfg.MCP.Auth.Audience, cfg.A2A.URL} {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Authenticate implements fw.Authn. A client certificate is checked first
// when auth.spiffe.x509 is set, then the bearer token.
func (a *Authn) Authenticate(r *http.Request) (*fw.Principal, error) {
	if a.x509 && r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		id, _, err := x509svid.Verify(r.TLS.PeerCertificates, a.src)
		if err == nil && id.TrustDomain() == a.td {
			return a.principal(id), nil
		}
		// A certificate that does not verify is not the end: the request
		// goes on to the bearer path with the same outcome as no certificate.
		a.log.Debug("spiffe client certificate refused", "error", err)
	}
	tok, ok := bearerToken(r)
	if !ok {
		return nil, fmt.Errorf("spiffe: no bearer token: %w", fw.ErrUnauthorized)
	}
	svid, err := jwtsvid.ParseAndValidate(tok, a.src, a.audiences)
	if err != nil {
		a.log.Debug("spiffe token refused", "error", err)
		return nil, fmt.Errorf("spiffe: %w: %w", fw.ErrUnauthorized, err)
	}
	if svid.ID.TrustDomain() != a.td {
		return nil, fmt.Errorf("spiffe: trust domain %s is not %s: %w", svid.ID.TrustDomain(), a.td, fw.ErrUnauthorized)
	}
	return a.principal(svid.ID), nil
}

// principal maps a SPIFFE ID to a principal. Groups stay empty: an access
// entry such as spiffe://example.org/ns/prod/* is a glob over the subject,
// which already names every workload under a path.
func (a *Authn) principal(id spiffeid.ID) *fw.Principal {
	return &fw.Principal{
		Subject: id.String(),
		Name:    strings.TrimPrefix(id.Path(), "/"),
		Kind:    fw.KindWorkload,
		Issuer:  id.TrustDomain().IDString(),
	}
}

// bearerToken returns the bearer token of a request.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

// Challenge implements fw.Authn.
func (a *Authn) Challenge(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="fw", error="invalid_token"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// NewTLS builds the listener TLS configuration of the spiffe provider. With
// the Workload API, the listener serves the X.509-SVID of this workload and
// follows its rotation. With a bundle file or URL, the listener serves
// tls.cert and tls.key. Either way, a client certificate must be an
// X.509-SVID of the trust domain, verified against the live bundle.
func NewTLS(ctx context.Context, cfg *fw.Config, _ fw.Secrets) (*tls.Config, error) {
	c := cfg.Auth.SPIFFE
	if c == nil {
		c = &fw.SPIFFEConfig{}
	}
	td, err := trustDomain(c.TrustDomain)
	if err != nil {
		return nil, fmt.Errorf("tls.provider spiffe: %w", err)
	}
	log := slog.Default().With("component", "spiffe")
	src, err := openSource(ctx, td, c.Bundle, c.Socket, false, true, log)
	if err != nil {
		return nil, err
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if src.path != "" || src.url != "" {
		if cfg.TLS.Cert == "" || cfg.TLS.Key == "" {
			return nil, errors.New("spiffe: tls.provider spiffe with auth.spiffe.bundle needs tls.cert and tls.key, because a bundle holds no certificate for this workload; set auth.spiffe.socket to serve the X.509-SVID from the Workload API instead")
		}
		cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
		if err != nil {
			return nil, fmt.Errorf("spiffe: load tls.cert and tls.key: %w", err)
		}
		tc.Certificates = []tls.Certificate{cert}
	} else {
		tc.GetCertificate = tlsconfig.GetCertificate(src)
	}
	if cfg.TLS.ClientCA != "" {
		log.Warn("tls.clientCA is not read by tls.provider spiffe; the trust bundle verifies client certificates")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.TLS.ClientAuth))
	if mode == "" {
		mode = "none"
		if c.X509 {
			mode = "request"
		}
	}
	verify := tlsconfig.VerifyPeerCertificate(src, tlsconfig.AuthorizeMemberOf(td))
	switch mode {
	case "none":
		tc.ClientAuth = tls.NoClientCert
	case "request":
		tc.ClientAuth = tls.RequestClientCert
		tc.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return nil
			}
			return verify(raw, chains)
		}
	case "require":
		tc.ClientAuth = tls.RequireAnyClientCert
		tc.VerifyPeerCertificate = verify
	default:
		return nil, fmt.Errorf("spiffe: tls.clientAuth %q is not none, request, or require", cfg.TLS.ClientAuth)
	}
	return tc, nil
}

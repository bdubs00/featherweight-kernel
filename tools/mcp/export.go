package mcp

// This file exports the OAuth helpers of this client for a package that
// runs an OAuth flow for its entries. The connections of the workbench do:
// they keep the grants of people and give this client their tokens through
// fw.PersonTokens (docs/ARCHITECTURE.md section 7.11).

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// Time limits of the OAuth requests of this client.
const (
	// OAuthTimeout bounds one metadata or token request.
	OAuthTimeout = oauthTimeout
	// RefreshMargin is how long before its expiry a cached token counts as
	// expired.
	RefreshMargin = refreshMargin
)

// ProtectedResource is the part of RFC 9728 metadata that this client
// reads.
type ProtectedResource = protectedResource

// CanonicalResource returns the canonical URI of an MCP server for the
// resource parameter (RFC 8707): lower-case scheme and host, no default
// port, no fragment, and no trailing slash on an empty path.
func CanonicalResource(u *url.URL) string { return canonicalResource(u) }

// ProtectedResourceURLs returns the RFC 9728 metadata URLs of an endpoint:
// the path-specific one first, then the root one.
func ProtectedResourceURLs(endpoint *url.URL) []string { return wellKnownResourceURLs(endpoint) }

// AuthServerMetadataURLs returns the metadata URLs to try for an issuer:
// RFC 8414 first, then OpenID Connect discovery.
func AuthServerMetadataURLs(issuer string) ([]string, error) { return authServerMetadataURLs(issuer) }

// ResourceCovers reports whether the resource in the metadata is the MCP
// endpoint or a prefix of it.
func ResourceCovers(meta, endpoint string) bool { return resourceCovers(meta, endpoint) }

// ExpiresIn reads expires_in as a number or a numeric string.
func ExpiresIn(v any) int64 { return expiresIn(v) }

// FetchJSON fetches a metadata document of at most 1 MiB with hc.
func FetchJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	return fetchJSON(ctx, hc, u, v)
}

// IsLocalHost reports whether a host is this machine or a cluster-local
// name, where plain http is allowed.
func IsLocalHost(host string) bool { return isLocalHost(host) }

// BaseTransport returns the HTTP transport of this client with the TLS
// settings tc, or the default settings when tc is nil.
func BaseTransport(tc *tls.Config) *http.Transport { return baseTransport(tc) }

// TLSConfig returns the TLS settings for an authorization server from the
// TLS settings of an entry: the system roots plus the CA bundle of the
// entry. It returns nil when the entry sets neither a CA nor insecure.
func TLSConfig(c fw.TLSConfig) (*tls.Config, error) { return oauthTLS(c) }

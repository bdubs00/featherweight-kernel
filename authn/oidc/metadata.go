package oidc

import (
	"net/url"
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// wellKnownPath is the prefix of the protected-resource document (RFC 9728).
const wellKnownPath = "/.well-known/oauth-protected-resource"

// ResourceMetadata builds the RFC 9728 protected-resource document that the
// MCP surface serves. It needs no verifier, so it works before discovery
// finishes.
//
// resource is mcp.auth.audience, or a2a.url when that is empty; the key is
// absent when neither is set. authorization_servers is
// mcp.auth.authorizationServers, or else the issuer. scopes_supported is
// auth.oidc.scopes when set. bearer_methods_supported is always ["header"].
func ResourceMetadata(cfg *fw.Config) map[string]any {
	doc := map[string]any{"bearer_methods_supported": []string{"header"}}
	if cfg == nil {
		return doc
	}
	if res := resourceOf(cfg); res != "" {
		doc["resource"] = res
	}
	servers := cleanList(cfg.MCP.Auth.AuthorizationServers)
	if len(servers) == 0 && cfg.Auth.OIDC != nil && strings.TrimSpace(cfg.Auth.OIDC.Issuer) != "" {
		servers = []string{strings.TrimSpace(cfg.Auth.OIDC.Issuer)}
	}
	if len(servers) > 0 {
		doc["authorization_servers"] = servers
	}
	if cfg.Auth.OIDC != nil {
		if scopes := cleanList(cfg.Auth.OIDC.Scopes); len(scopes) > 0 {
			doc["scopes_supported"] = scopes
		}
	}
	return doc
}

// ResourceMetadataURL returns the URL of the protected-resource document
// that the challenge names, or "" when neither mcp.auth.audience nor
// a2a.url is set. As RFC 9728 says, the well-known path comes right after
// the host, and the path of the resource follows it: the resource
// https://agent.example.com/mcp has its document at
// https://agent.example.com/.well-known/oauth-protected-resource/mcp.
func ResourceMetadataURL(cfg *fw.Config) string {
	if cfg == nil {
		return ""
	}
	res := resourceOf(cfg)
	if res == "" {
		return ""
	}
	u, err := url.Parse(res)
	if err != nil || u.Host == "" {
		return ""
	}
	out := url.URL{Scheme: u.Scheme, Host: u.Host, Path: wellKnownPath}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		out.Path += p
	}
	return out.String()
}

// resourceOf returns the resource identifier: mcp.auth.audience, or
// a2a.url when that is empty.
func resourceOf(cfg *fw.Config) string {
	if res := strings.TrimSpace(cfg.MCP.Auth.Audience); res != "" {
		return res
	}
	return strings.TrimSpace(cfg.A2A.URL)
}

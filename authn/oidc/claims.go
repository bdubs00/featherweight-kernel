package oidc

import (
	"strings"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// nameClaims are the claims tried for Principal.Name, in order, when no
// nameClaim is configured or the configured claim is empty.
var nameClaims = []string{"email", "preferred_username", "name", "sub"}

// principalFromClaims maps the claims of a verified token to a principal.
//
// Subject is sub. Name is the claim nameClaim when
// set and present, else the first of email, preferred_username, name, and
// sub that is set. Groups come from groupsClaim, or from groups, or from
// roles when the token has no groups claim and no groupsClaim is
// configured. Issuer is iss. Scopes come from the scope claim split on
// spaces, or from the scp list. Kind is user, or workload when the token
// is a client-credentials token (see isWorkload).
func principalFromClaims(claims map[string]any, groupsClaim, nameClaim string) *fw.Principal {
	p := &fw.Principal{
		Subject: stringClaim(claims, "sub"),
		Issuer:  stringClaim(claims, "iss"),
		Kind:    fw.KindUser,
	}
	if nameClaim != "" {
		p.Name = stringClaim(claims, nameClaim)
	}
	for _, c := range nameClaims {
		if p.Name != "" {
			break
		}
		p.Name = stringClaim(claims, c)
	}
	switch {
	case groupsClaim != "":
		p.Groups = stringsClaim(claims, groupsClaim)
	default:
		if _, ok := lookupClaim(claims, "groups"); ok {
			p.Groups = stringsClaim(claims, "groups")
		} else {
			p.Groups = stringsClaim(claims, "roles")
		}
	}
	if s := stringClaim(claims, "scope"); s != "" {
		p.Scopes = strings.Fields(s)
	} else {
		p.Scopes = stringsClaim(claims, "scp")
	}
	if isWorkload(claims) {
		p.Kind = fw.KindWorkload
	}
	return p
}

// isWorkload reports whether the token comes from the client-credentials
// grant, where no person is behind it. Such a token names the client as
// its subject: the client_id claim (RFC 9068) equals sub, or the azp claim
// (the authorized party) equals sub and the token has no email claim,
// which is how a service account token looks. A token of a person has a
// user id as sub, which differs from the client id, and often an email.
func isWorkload(claims map[string]any) bool {
	sub := stringClaim(claims, "sub")
	if sub == "" {
		return false
	}
	if stringClaim(claims, "client_id") == sub {
		return true
	}
	return stringClaim(claims, "azp") == sub && stringClaim(claims, "email") == ""
}

// lookupClaim returns a claim by name. A name with dots that is not a
// claim itself walks into nested objects, so realm_access.roles reads the
// roles list inside the realm_access object.
func lookupClaim(claims map[string]any, name string) (any, bool) {
	if v, ok := claims[name]; ok {
		return v, true
	}
	if !strings.Contains(name, ".") {
		return nil, false
	}
	var cur any = claims
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// stringClaim returns a claim as a trimmed string, or "" when it is absent
// or not a string.
func stringClaim(claims map[string]any, name string) string {
	v, ok := lookupClaim(claims, name)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// stringsClaim returns a claim that holds a list of strings, or one
// string, as a list. Items that are not strings and empty items are
// dropped.
func stringsClaim(claims map[string]any, name string) []string {
	v, ok := lookupClaim(claims, name)
	if !ok {
		return nil
	}
	switch v := v.(type) {
	case string:
		if s := strings.TrimSpace(v); s != "" {
			return []string{s}
		}
	case []string:
		return cleanList(v)
	case []any:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
		return out
	}
	return nil
}

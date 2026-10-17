package fw

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Redacted replaces a secret in a ledger event.
const Redacted = "[redacted]"

// minSecretLen is the length of the shortest value that the redactor
// replaces. A shorter value, such as a test token, matches too much text.
const minSecretLen = 8

// secretShapes match text with the shape of a known secret. The match
// becomes the replacement, in which ${1} is the first group. A URL keeps
// its scheme and host and loses its user information: always for http and
// https, which carry a token there, and for another scheme when the user
// information holds a password, so that ssh://git@host keeps its user.
var secretShapes = []struct{ pattern, repl string }{
	{`-----BEGIN[A-Z0-9 ]*PRIVATE KEY-----[^"]*?-----END[A-Z0-9 ]*PRIVATE KEY-----`, Redacted},
	{`\bsk-ant-[A-Za-z0-9_-]{20,}`, Redacted},
	{`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{32,}`, Redacted},
	{`\bgh[pousr]_[A-Za-z0-9]{30,}`, Redacted},
	{`\bgithub_pat_[A-Za-z0-9_]{40,}`, Redacted},
	{`\bglpat-[A-Za-z0-9_-]{20,}`, Redacted},
	{`\bxox[abposr]-[A-Za-z0-9-]{10,}`, Redacted},
	{`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, Redacted},
	{`\bAIza[0-9A-Za-z_-]{35}`, Redacted},
	{`\bhf_[A-Za-z0-9]{30,}`, Redacted},
	{`\bnpm_[A-Za-z0-9]{36}`, Redacted},
	{`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`, Redacted},
	{`(?i)\b(bearer\s+)[A-Za-z0-9._~+/-]{16,}=*`, "${1}" + Redacted},
	{`(?i)\b(https?://)[^/?#\s"'@]+@`, "${1}" + Redacted + "@"},
	{`(?i)\b([a-z][a-z0-9+.-]*://)[^/?#\s"':@]+:[^/?#\s"'@]*@`, "${1}" + Redacted + "@"},
}

// shapes are the compiled secretShapes. anyShape is every shape in one
// pattern without \b, for raw JSON, in which an escape such as "\n" hides
// a word boundary.
var shapes, anyShape = compileShapes()

func compileShapes() ([]*regexp.Regexp, *regexp.Regexp) {
	var out []*regexp.Regexp
	var all []string
	for _, s := range secretShapes {
		out = append(out, regexp.MustCompile(s.pattern))
		all = append(all, "(?:"+strings.ReplaceAll(s.pattern, `\b`, "")+")")
	}
	return out, regexp.MustCompile(strings.Join(all, "|"))
}

// notSecret holds the last words of a variable name that names where a
// secret is, not the secret itself.
var notSecret = map[string]bool{"FILE": true, "PATH": true, "DIR": true, "ENV": true, "NAME": true, "ID": true, "MODE": true, "TYPE": true, "URL": true, "HEADER": true}

// secretName matches the name of a variable that holds a secret, such as
// OPENAI_API_KEY, FW_API_TOKEN, or DB_PASSWORD.
var secretName = regexp.MustCompile(`(?i)(^|_)(API_?KEY|KEY|TOKEN|SECRET|PASSWORD|PASSWD|PASS|PAT|CREDENTIALS?)($|_)`)

// isSecretName reports whether a variable name holds a secret. A name that
// ends in a word such as _FILE, _PATH, or _ENV names where the secret is,
// and is not one.
func isSecretName(name string) bool {
	if !secretName.MatchString(name) {
		return false
	}
	last := name[strings.LastIndexByte(name, '_')+1:]
	return !notSecret[strings.ToUpper(last)]
}

// redactor removes secrets from event data before the kernel appends the
// event. It knows two kinds of secret: the values that fw resolved through
// Secrets or found in a variable with a secret name, and text with the
// shape of a known secret, such as a PEM private key, a GitHub token, or
// the user information of a URL.
type redactor struct {
	mu     sync.RWMutex
	values []string // longest first, so a value that holds another goes first
}

// newRedactor returns a redactor that knows the secret values of the
// environment, given as NAME=VALUE pairs.
func newRedactor(environ []string) *redactor {
	r := &redactor{}
	for _, kv := range environ {
		if name, value, ok := strings.Cut(kv, "="); ok && isSecretName(name) {
			r.add(value)
		}
	}
	return r
}

// add records a secret value.
func (r *redactor) add(v string) {
	v = strings.TrimSpace(v)
	if len(v) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.values, v) {
		return
	}
	r.values = append(r.values, v)
	slices.SortFunc(r.values, func(a, b string) int { return len(b) - len(a) })
}

// text returns s with every secret replaced.
func (r *redactor) text(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	for _, v := range r.values {
		s = strings.ReplaceAll(s, v, Redacted)
	}
	r.mu.RUnlock()
	for i, re := range shapes {
		s = re.ReplaceAllString(s, secretShapes[i].repl)
	}
	return s
}

// holds reports whether raw JSON can hold a secret. It is the fast path:
// most events hold none, and the kernel appends them unchanged.
func (r *redactor) holds(raw []byte) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.values {
		if bytes.Contains(raw, []byte(v)) || bytes.Contains(raw, jsonText(v)) {
			return true
		}
	}
	return anyShape.Match(raw)
}

// jsonText returns v as it appears inside a JSON string.
func jsonText(v string) []byte {
	b, _ := json.Marshal(v)
	return b[1 : len(b)-1]
}

// data returns event data with every secret replaced in its strings. It
// leaves an opaque block alone, because a provider signs it, and it leaves
// the bytes of an image or a document alone.
func (r *redactor) data(raw json.RawMessage) json.RawMessage {
	if r == nil || len(raw) == 0 || !r.holds(raw) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(r.walk(v))
	if err != nil {
		return raw
	}
	return out
}

func (r *redactor) walk(v any) any {
	switch t := v.(type) {
	case string:
		return r.text(t)
	case []any:
		for i := range t {
			t[i] = r.walk(t[i])
		}
	case map[string]any:
		typ := t["type"]
		if typ == BlockOpaque {
			return t
		}
		for k, x := range t {
			if k == "data" && (typ == BlockImage || typ == BlockDocument) {
				continue
			}
			t[k] = r.walk(x)
		}
	}
	return v
}

// recordSecrets passes each value that Secrets resolves to the redactor.
type recordSecrets struct {
	Secrets
	r *redactor
}

// Get implements Secrets.
func (s recordSecrets) Get(ctx context.Context, name string) (string, error) {
	v, err := s.Secrets.Get(ctx, name)
	if err == nil {
		s.r.add(v)
	}
	return v, err
}

// Redact returns s with every secret that the agent knows replaced, as the
// kernel does for each ledger event. Use it before text goes to a log line.
func (a *Agent) Redact(s string) string { return a.redact.text(s) }
